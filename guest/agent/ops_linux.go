//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

type linuxVsockListener struct {
	fd int
}

func listenVsock(port uint32) (vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &linuxVsockListener{fd: fd}, nil
}

// Compile-time guard that the portable host CID constant matches Linux's.
var _ [0]int = [unix.VMADDR_CID_HOST - vsockHostCID]int{}

func (l *linuxVsockListener) Accept() (io.ReadWriteCloser, error) {
	for {
		c, sa, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, err
		}
		// Only accept connections from the host hypervisor (VMADDR_CID_HOST).
		// Guest-local processes and build containers must never reach the agent.
		vm, ok := sa.(*unix.SockaddrVM)
		if !ok || !isAllowedVsockCID(vm.CID) {
			log.Printf("rejecting vsock peer %T (%+v): want CID %d", sa, sa, unix.VMADDR_CID_HOST)
			_ = unix.Close(c)
			continue
		}
		// Nonblocking so Go's netpoller drives Read/Write and Close wakes
		// blocked readers cleanly without needing net.FileConn.
		if err := unix.SetNonblock(c, true); err != nil {
			_ = unix.Close(c)
			continue
		}
		return os.NewFile(uintptr(c), "vsock"), nil
	}
}

func (l *linuxVsockListener) Close() error {
	return unix.Close(l.fd)
}

type linuxOps struct{}

func defaultOps() sysOps { return linuxOps{} }

func (linuxOps) HostInfo() hostInfo {
	var info hostInfo
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		info.Kernel = unix.ByteSliceToString(u.Release[:])
		info.Machine = unix.ByteSliceToString(u.Machine[:])
	}
	if b, err := os.ReadFile("/proc/sys/fs/binfmt_misc/rosetta"); err == nil {
		info.Rosetta = strings.HasPrefix(string(b), "enabled")
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			info.Uptime, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	return info
}

func (linuxOps) MountDisk(format bool) (guestproto.DiskStatus, error) {
	if fsType, mounted := findMount(guestproto.DataMount); mounted {
		return statDisk(guestproto.DataMount, fsType, false)
	}

	// Wait briefly for the virtio-blk device node to appear in devtmpfs.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(guestproto.DataDevice); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return guestproto.DiskStatus{}, fmt.Errorf("data disk %s not found", guestproto.DataDevice)
		}
		time.Sleep(50 * time.Millisecond)
	}

	fsType, err := probeFS(guestproto.DataDevice)
	if err != nil {
		return guestproto.DiskStatus{}, err
	}

	formatted := false
	if fsType == "" {
		if !format {
			return guestproto.DiskStatus{}, fmt.Errorf("%s has no filesystem and format is false", guestproto.DataDevice)
		}
		out, err := exec.Command(
			"/sbin/mke2fs",
			"-t", "ext4",
			"-L", guestproto.DataLabel,
			"-E", "lazy_itable_init=1,lazy_journal_init=1",
			"-q", "-F",
			guestproto.DataDevice,
		).CombinedOutput()
		if err != nil {
			return guestproto.DiskStatus{}, fmt.Errorf("mke2fs %s: %v: %s", guestproto.DataDevice, err, strings.TrimSpace(string(out)))
		}
		fsType = "ext4"
		formatted = true
	} else if fsType == "ext4" || fsType == "ext3" || fsType == "ext2" {
		cmd := exec.Command("/sbin/e2fsck", "-p", guestproto.DataDevice)
		out, err := cmd.CombinedOutput()
		if err != nil {
			var ee *exec.ExitError
			// e2fsck exit codes: 0 clean, 1 errors corrected, 2 corrected (reboot).
			// Bit 4 and above indicate uncorrected or operational errors.
			if !errors.As(err, &ee) || ee.ExitCode() >= 4 {
				return guestproto.DiskStatus{}, fmt.Errorf("e2fsck %s: %v: %s", guestproto.DataDevice, err, strings.TrimSpace(string(out)))
			}
		}
	}

	if err := os.MkdirAll(guestproto.DataMount, 0o755); err != nil {
		return guestproto.DiskStatus{}, fmt.Errorf("mkdir %s: %w", guestproto.DataMount, err)
	}
	if err := unix.Mount(guestproto.DataDevice, guestproto.DataMount, fsType, 0, ""); err != nil {
		return guestproto.DiskStatus{}, fmt.Errorf("mount %s on %s (%s): %w", guestproto.DataDevice, guestproto.DataMount, fsType, err)
	}

	buildkitRoot := filepath.Join(guestproto.DataMount, "buildkit")
	_ = os.MkdirAll(buildkitRoot, 0o755)
	if _, err := os.Lstat("/var/lib/buildkit"); errors.Is(err, os.ErrNotExist) {
		_ = os.Symlink(buildkitRoot, "/var/lib/buildkit")
	}

	return statDisk(guestproto.DataMount, fsType, formatted)
}

func probeFS(dev string) (string, error) {
	out, err := exec.Command("/sbin/blkid", "-o", "value", "-s", "TYPE", dev).CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		// blkid exits 2 when the device has no recognized filesystem.
		if errors.As(err, &ee) && ee.ExitCode() == 2 {
			return "", nil
		}
		return "", fmt.Errorf("blkid %s: %v: %s", dev, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func findMount(target string) (fsType string, mounted bool) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == target {
			return fields[2], true
		}
	}
	return "", false
}

func statDisk(mountPoint, fsType string, formatted bool) (guestproto.DiskStatus, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(mountPoint, &st); err != nil {
		return guestproto.DiskStatus{}, fmt.Errorf("statfs %s: %w", mountPoint, err)
	}
	bsize := int64(st.Bsize)
	return guestproto.DiskStatus{
		Formatted: formatted,
		Size:      int64(st.Blocks) * bsize,
		Free:      int64(st.Bavail) * bsize,
		FSType:    fsType,
	}, nil
}

func (linuxOps) UnmountDisk() error {
	if _, mounted := findMount(guestproto.DataMount); !mounted {
		return nil
	}
	unix.Sync()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		err := unix.Unmount(guestproto.DataMount, 0)
		if err == nil || errors.Is(err, unix.EINVAL) {
			return nil
		}
		lastErr = err
		if !errors.Is(err, unix.EBUSY) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// If still busy, remount read-only so the journal flushes cleanly.
	if err := unix.Mount("", guestproto.DataMount, "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err == nil {
		unix.Sync()
		return nil
	}
	return fmt.Errorf("umount %s: %w", guestproto.DataMount, lastErr)
}

func (linuxOps) Sync() {
	unix.Sync()
}

func (linuxOps) PowerOff() {
	unix.Sync()
	_ = exec.Command("/bin/busybox", "poweroff", "-f").Run()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}

func (linuxOps) Dial(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, 5*time.Second)
}

type linuxServiceProc struct {
	cmd     *exec.Cmd
	done    chan error
	logFile *os.File
	tail    *tailBuffer
}

func (p *linuxServiceProc) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *linuxServiceProc) Done() <-chan error { return p.done }

func (p *linuxServiceProc) Terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	return unix.Kill(-p.cmd.Process.Pid, unix.SIGTERM)
}

func (p *linuxServiceProc) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return unix.Kill(-p.cmd.Process.Pid, unix.SIGKILL)
}

func (p *linuxServiceProc) LogTail() string {
	return p.tail.String()
}

func (linuxOps) StartService(name string) (serviceProc, error) {
	switch name {
	case guestproto.ServiceBuildkitd:
		return startBuildkitd()
	default:
		return nil, fmt.Errorf("unknown service %s", name)
	}
}

func startBuildkitd() (serviceProc, error) {
	assetsBase := filepath.Join("/mnt", guestproto.AssetsTag, "buildkit")
	binDir := filepath.Join(assetsBase, "bin")
	bin := filepath.Join(binDir, "buildkitd")
	certs := filepath.Join(assetsBase, "certs", "ca-certificates.crt")
	configPath := filepath.Join(assetsBase, "buildkitd.toml")
	rootDir := filepath.Join(guestproto.DataMount, "buildkit")

	if err := os.MkdirAll(filepath.Dir(guestproto.BuildkitdSocket), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir socket dir: %w", err)
	}
	_ = os.Remove(guestproto.BuildkitdSocket)
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir buildkit root: %w", err)
	}

	args := []string{
		"--addr", "unix://" + guestproto.BuildkitdSocket,
		"--root", rootDir,
		"--oci-worker-platform", "linux/amd64",
		"--oci-worker-platform", "linux/arm64",
	}
	if _, err := os.Stat(configPath); err == nil {
		args = append([]string{"--config", configPath}, args...)
	}

	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = buildServiceEnv(binDir, certs)

	tail := newTailBuffer(2048)
	var out io.Writer = tail
	logFile, err := os.OpenFile("/run/buildkitd.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		out = io.MultiWriter(logFile, tail)
	}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("start buildkitd: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		done <- err
		close(done)
	}()

	return &linuxServiceProc{
		cmd:     cmd,
		done:    done,
		logFile: logFile,
		tail:    tail,
	}, nil
}

func buildServiceEnv(binDir, certFile string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "SSL_CERT_FILE=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH="+binDir+":/sbin:/bin")
	if _, err := os.Stat(certFile); err == nil {
		env = append(env, "SSL_CERT_FILE="+certFile)
	}
	return env
}
