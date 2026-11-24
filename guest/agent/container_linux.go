//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

type linuxContainerOps struct {
	mu           sync.Mutex
	started      bool
	running      bool
	paused       bool
	spec         guestproto.ContainerSpec
	pid1         int
	exitCode     int
	exitSignal   string
	exitErr      string
	hub          *outputHub
	done         chan struct{}
	ptyMaster    *os.File
	stdinPipe    io.WriteCloser
	stdinMu      sync.Mutex
	stdinClosed  bool
	layerMounts  []string
	loopDevs     []string
	volMounts    []string
	innerMounts  []string
	rootfsUp     bool
	cgroupActive bool
}

func defaultContainerOps() containerOps {
	return &linuxContainerOps{}
}

func (c *linuxContainerOps) stateLocked() guestproto.ContainerState {
	return guestproto.ContainerState{
		Running:  c.running,
		Paused:   c.paused,
		PID:      c.pid1,
		ExitCode: c.exitCode,
		Signal:   c.exitSignal,
		Error:    c.exitErr,
	}
}

func (c *linuxContainerOps) rootDirLocked() string {
	return ctrRootfs
}

func (c *linuxContainerOps) Start(spec guestproto.ContainerSpec) (guestproto.ContainerState, error) {
	if err := validateContainerSpec(&spec); err != nil {
		return guestproto.ContainerState{}, errWithCode(guestproto.CodeBadRequest, err)
	}

	c.mu.Lock()
	if c.started {
		st := c.stateLocked()
		c.mu.Unlock()
		return st, errWithCode(guestproto.CodeFailed, errors.New("container already started in this VM"))
	}
	c.started = true
	c.spec = spec
	c.mu.Unlock()

	cleanupOnFail := true
	defer func() {
		if cleanupOnFail {
			c.cleanupResources()
			c.mu.Lock()
			c.started = false
			c.running = false
			c.paused = false
			c.pid1 = 0
			c.mu.Unlock()
		}
	}()

	// 1. Mount EROFS layers (fast path virtio-blk or overflow loop.ko).
	if err := c.mountLayers(spec.Layers); err != nil {
		return guestproto.ContainerState{}, err
	}

	// 2. Assemble overlayfs (or read-only overlay/bind when ReadonlyRootfs).
	if err := c.mountRootfs(spec.ReadonlyRootfs); err != nil {
		return guestproto.ContainerState{}, err
	}

	// 3. Populate /dev, /etc/{hostname,hosts,resolv.conf}, and user mounts.
	if err := c.setupSpecialAndUserMounts(spec); err != nil {
		return guestproto.ContainerState{}, err
	}

	// 4. Resolve user against container rootfs before pivot_root.
	u, err := resolveUser(ctrRootfs, spec.User)
	if err != nil {
		return guestproto.ContainerState{}, errWithCode(guestproto.CodeBadRequest, err)
	}

	// 5. Configure cgroup v2 (/sys/fs/cgroup/gvx-ctr).
	if err := c.setupCgroup(spec); err != nil {
		return guestproto.ContainerState{}, err
	}

	// 6. Prepare __ctr-init config file + CLOEXEC error pipe.
	if err := os.MkdirAll(ctrRunDir, 0o700); err != nil {
		return guestproto.ContainerState{}, fmt.Errorf("mkdir %s: %w", ctrRunDir, err)
	}
	initCfg := ctrInitConfig{
		Argv:           spec.Argv,
		Env:            mergeEnv(nil, spec.Env, u.Home, spec.TTY),
		Workdir:        spec.Workdir,
		Hostname:       spec.Hostname,
		Privileged:     spec.Privileged,
		ReadonlyRootfs: spec.ReadonlyRootfs,
		User:           u,
	}
	cfgBytes, err := json.Marshal(initCfg)
	if err != nil {
		return guestproto.ContainerState{}, err
	}
	cfgPath := filepath.Join(ctrRunDir, "init.json")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		return guestproto.ContainerState{}, fmt.Errorf("write init config: %w", err)
	}
	cfgFile, err := os.Open(cfgPath)
	if err != nil {
		return guestproto.ContainerState{}, err
	}
	defer cfgFile.Close()
	defer os.Remove(cfgPath)

	errPipeR, errPipeW, err := os.Pipe()
	if err != nil {
		return guestproto.ContainerState{}, fmt.Errorf("init error pipe: %w", err)
	}
	defer errPipeR.Close()

	exePath, err := os.Executable()
	if err != nil {
		exePath = "/bin/gvx-agent"
	}

	cmd := exec.Command(exePath, ctrInitArg)
	cmd.ExtraFiles = []*os.File{cfgFile, errPipeW}

	cgFD, err := unix.Open(ctrCgroupDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = errPipeW.Close()
		return guestproto.ContainerState{}, fmt.Errorf("open cgroup dir: %w", err)
	}
	defer unix.Close(cgFD)

	sysAttr := &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC,
		UseCgroupFD: true,
		CgroupFD:    cgFD,
	}

	var (
		ptyMaster *os.File
		ptySlave  *os.File
		stdoutR   *os.File
		stdoutW   *os.File
		stderrR   *os.File
		stderrW   *os.File
		stdinR    *os.File
		stdinW    *os.File
	)
	closeSetupFDs := func() {
		for _, f := range []*os.File{ptyMaster, ptySlave, stdoutR, stdoutW, stderrR, stderrW, stdinR, stdinW} {
			if f != nil {
				_ = f.Close()
			}
		}
	}

	if spec.TTY {
		ptyMaster, ptySlave, err = openPTY(spec.Rows, spec.Cols)
		if err != nil {
			_ = errPipeW.Close()
			return guestproto.ContainerState{}, fmt.Errorf("open pty: %w", err)
		}
		if u.UID != 0 || u.GID != 0 {
			_ = unix.Fchown(int(ptySlave.Fd()), int(u.UID), int(u.GID))
		}
		cmd.Stdin = ptySlave
		cmd.Stdout = ptySlave
		cmd.Stderr = ptySlave
		sysAttr.Setsid = true
		sysAttr.Setctty = true
		sysAttr.Ctty = 0
	} else {
		stdoutR, stdoutW, err = os.Pipe()
		if err != nil {
			_ = errPipeW.Close()
			closeSetupFDs()
			return guestproto.ContainerState{}, err
		}
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			_ = errPipeW.Close()
			closeSetupFDs()
			return guestproto.ContainerState{}, err
		}
		cmd.Stdout = stdoutW
		cmd.Stderr = stderrW
		if spec.Interactive {
			stdinR, stdinW, err = os.Pipe()
			if err != nil {
				_ = errPipeW.Close()
				closeSetupFDs()
				return guestproto.ContainerState{}, err
			}
			cmd.Stdin = stdinR
		}
	}
	cmd.SysProcAttr = sysAttr

	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		_ = errPipeW.Close()
		closeSetupFDs()
		return guestproto.ContainerState{}, fmt.Errorf("start container init: %w", err)
	}

	// Close child-side FDs in parent immediately.
	_ = errPipeW.Close()
	if ptySlave != nil {
		_ = ptySlave.Close()
		ptySlave = nil
	}
	if stdoutW != nil {
		_ = stdoutW.Close()
		stdoutW = nil
	}
	if stderrW != nil {
		_ = stderrW.Close()
		stderrW = nil
	}
	if stdinR != nil {
		_ = stdinR.Close()
		stdinR = nil
	}

	// Wait for __ctr-init's CLOEXEC error pipe: EOF means unix.Exec succeeded.
	initErrBytes, _ := io.ReadAll(io.LimitReader(errPipeR, 64<<10))
	if len(initErrBytes) > 0 {
		_ = cmd.Wait()
		closeSetupFDs()
		return guestproto.ContainerState{}, fmt.Errorf("container init: %s", strings.TrimSpace(string(initErrBytes)))
	}

	hub := newOutputHub(defaultOutputBufBytes)
	done := make(chan struct{})

	c.mu.Lock()
	c.running = true
	c.paused = false
	c.pid1 = cmd.Process.Pid
	c.hub = hub
	c.done = done
	c.ptyMaster = ptyMaster
	c.stdinPipe = stdinW
	c.stdinClosed = false
	state := c.stateLocked()
	c.mu.Unlock()

	cleanupOnFail = false

	// Pump stdout/stderr (or PTY master) into hub and wait for PID 1 exit.
	var ioWG sync.WaitGroup
	if ptyMaster != nil {
		ioWG.Add(1)
		go func() {
			defer ioWG.Done()
			pumpReaderToHub(ptyMaster, hub, guestproto.FrameStdout, true)
		}()
	} else {
		ioWG.Add(2)
		go func() {
			defer ioWG.Done()
			defer stdoutR.Close()
			pumpReaderToHub(stdoutR, hub, guestproto.FrameStdout, false)
		}()
		go func() {
			defer ioWG.Done()
			defer stderrR.Close()
			pumpReaderToHub(stderrR, hub, guestproto.FrameStderr, false)
		}()
	}

	go func() {
		waitErr := cmd.Wait()
		elapsed := time.Since(startTime)
		// Wait for all stdout/stderr pipes to finish draining before emitting FrameExit.
		ioWG.Wait()

		code, sigName, errMsg := extractExitInfo(cmd.ProcessState, waitErr)
		ex := guestproto.Exit{
			Code:    code,
			Elapsed: elapsed.Nanoseconds(),
			Error:   errMsg,
			Signal:  sigName,
		}

		c.stdinMu.Lock()
		c.mu.Lock()
		c.running = false
		c.paused = false
		c.exitCode = code
		c.exitSignal = sigName
		c.exitErr = errMsg
		pm := c.ptyMaster
		sp := c.stdinPipe
		c.ptyMaster = nil
		c.stdinPipe = nil
		c.mu.Unlock()

		c.stdinClosed = true
		if pm != nil {
			_ = pm.Close()
		}
		if sp != nil {
			_ = sp.Close()
		}
		c.stdinMu.Unlock()

		hub.Finish(ex)
		close(done)
	}()

	return state, nil
}

func pumpReaderToHub(r io.Reader, hub *outputHub, kind byte, isPTY bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			hub.Append(kind, buf[:n])
		}
		if err != nil {
			if isPTY && errors.Is(err, syscall.EIO) {
				return
			}
			return
		}
	}
}

func (c *linuxContainerOps) mountLayers(layers []guestproto.LayerSource) error {
	for i, src := range layers {
		mp := fmt.Sprintf("%s%d", ctrLayerBase, i)
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return fmt.Errorf("mkdir layer %s: %w", mp, err)
		}

		var dev string
		var loopFD int = -1
		if src.Device != "" {
			if err := waitForPath(src.Device, 2*time.Second); err != nil {
				return fmt.Errorf("layer %d device %s: %w", i, src.Device, err)
			}
			dev = src.Device
		} else {
			if err := waitForPath(src.File, 2*time.Second); err != nil {
				return fmt.Errorf("layer %d file %s: %w", i, src.File, err)
			}
			var err error
			dev, loopFD, err = attachLoopReadOnly(src.File)
			if err != nil {
				return fmt.Errorf("layer %d loop %s: %w", i, src.File, err)
			}
			c.mu.Lock()
			c.loopDevs = append(c.loopDevs, dev)
			c.mu.Unlock()
		}

		err := unix.Mount(dev, mp, "erofs", unix.MS_RDONLY, "")
		if loopFD >= 0 {
			_ = unix.Close(loopFD)
		}
		if err != nil {
			return fmt.Errorf("mount erofs %s on %s: %w", dev, mp, err)
		}
		c.mu.Lock()
		c.layerMounts = append(c.layerMounts, mp)
		c.mu.Unlock()
	}
	return nil
}

func attachLoopReadOnly(filePath string) (string, int, error) {
	if err := waitForPath("/dev/loop-control", 2*time.Second); err != nil {
		return "", -1, fmt.Errorf("/dev/loop-control unavailable: %w", err)
	}
	ctlFD, err := unix.Open("/dev/loop-control", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", -1, fmt.Errorf("open /dev/loop-control: %w", err)
	}
	defer unix.Close(ctlFD)

	num, err := unix.IoctlRetInt(ctlFD, unix.LOOP_CTL_GET_FREE)
	if err != nil {
		return "", -1, fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
	}
	devPath := fmt.Sprintf("/dev/loop%d", num)
	if _, err := os.Stat(devPath); errors.Is(err, os.ErrNotExist) {
		if mknodErr := unix.Mknod(devPath, unix.S_IFBLK|0o660, int(unix.Mkdev(7, uint32(num)))); mknodErr != nil && !errors.Is(mknodErr, unix.EEXIST) {
			return "", -1, fmt.Errorf("mknod %s: %w", devPath, mknodErr)
		}
	}

	imgFD, err := unix.Open(filePath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", -1, fmt.Errorf("open %s: %w", filePath, err)
	}
	defer unix.Close(imgFD)

	loopFD, err := unix.Open(devPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", -1, fmt.Errorf("open %s: %w", devPath, err)
	}
	if err := unix.IoctlSetInt(loopFD, unix.LOOP_SET_FD, imgFD); err != nil {
		_ = unix.Close(loopFD)
		return "", -1, fmt.Errorf("LOOP_SET_FD %s -> %s: %w", filePath, devPath, err)
	}
	info := &unix.LoopInfo64{
		Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR,
	}
	copy(info.File_name[:], filePath)
	if err := unix.IoctlLoopSetStatus64(loopFD, info); err != nil {
		_, _ = unix.IoctlRetInt(loopFD, unix.LOOP_CLR_FD)
		_ = unix.Close(loopFD)
		return "", -1, fmt.Errorf("LOOP_SET_STATUS64 %s: %w", devPath, err)
	}
	return devPath, loopFD, nil
}

func (c *linuxContainerOps) mountRootfs(readonly bool) error {
	if err := os.MkdirAll(ctrRootfs, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", ctrRootfs, err)
	}
	c.mu.Lock()
	mounts := append([]string(nil), c.layerMounts...)
	c.mu.Unlock()

	lower := buildLowerDir(mounts)
	if readonly {
		if len(mounts) == 1 {
			if err := unix.Mount(mounts[0], ctrRootfs, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("bind mount single ro layer: %w", err)
			}
			if err := unix.Mount("", ctrRootfs, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("remount single ro layer: %w", err)
			}
		} else {
			if err := unix.Mount("overlay", ctrRootfs, "overlay", unix.MS_RDONLY, "lowerdir="+lower); err != nil {
				return fmt.Errorf("mount ro overlayfs: %w", err)
			}
		}
	} else {
		if err := os.MkdirAll(ctrUpperDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", ctrUpperDir, err)
		}
		// Clean stale overlay workdir from prior runs while keeping upperdir intact.
		_ = os.RemoveAll(ctrWorkDir)
		if err := os.MkdirAll(ctrWorkDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", ctrWorkDir, err)
		}
		opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, ctrUpperDir, ctrWorkDir)
		if err := unix.Mount("overlay", ctrRootfs, "overlay", 0, opts); err != nil {
			return fmt.Errorf("mount overlayfs: %w", err)
		}
	}
	c.mu.Lock()
	c.rootfsUp = true
	c.mu.Unlock()
	return nil
}

func (c *linuxContainerOps) recordInnerMount(target string) {
	c.mu.Lock()
	c.innerMounts = append(c.innerMounts, target)
	c.mu.Unlock()
}

func (c *linuxContainerOps) setupSpecialAndUserMounts(spec guestproto.ContainerSpec) error {
	// Ensure /proc, /sys, /dev mountpoints exist when rootfs is writable.
	if !spec.ReadonlyRootfs {
		for _, d := range []string{"proc", "sys", "dev", "etc"} {
			_ = os.MkdirAll(filepath.Join(ctrRootfs, d), 0o755)
		}
	}

	// Set up /dev tmpfs + minimal device nodes + /dev/pts + /dev/shm.
	devDir := filepath.Join(ctrRootfs, "dev")
	if err := unix.Mount("tmpfs", devDir, "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=0755,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev tmpfs: %w", err)
	}
	c.recordInnerMount(devDir)

	ptsDir := filepath.Join(devDir, "pts")
	if err := os.MkdirAll(ptsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir /dev/pts: %w", err)
	}
	if err := unix.Mount("devpts", ptsDir, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return fmt.Errorf("mount /dev/pts: %w", err)
	}
	c.recordInnerMount(ptsDir)
	_ = os.Symlink("pts/ptmx", filepath.Join(devDir, "ptmx"))

	shmDir := filepath.Join(devDir, "shm")
	if err := os.MkdirAll(shmDir, 0o1777); err != nil {
		return fmt.Errorf("mkdir /dev/shm: %w", err)
	}
	if err := unix.Mount("tmpfs", shmDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=1777,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev/shm: %w", err)
	}
	c.recordInnerMount(shmDir)

	charDevs := []struct {
		rel   string
		major uint32
		minor uint32
		mode  uint32
	}{
		{"null", 1, 3, 0o666},
		{"zero", 1, 5, 0o666},
		{"full", 1, 7, 0o666},
		{"random", 1, 8, 0o666},
		{"urandom", 1, 9, 0o666},
		{"tty", 5, 0, 0o666},
	}
	if spec.Privileged {
		_ = os.MkdirAll(filepath.Join(devDir, "net"), 0o755)
		charDevs = append(charDevs,
			struct {
				rel   string
				major uint32
				minor uint32
				mode  uint32
			}{"net/tun", 10, 200, 0o666},
			struct {
				rel   string
				major uint32
				minor uint32
				mode  uint32
			}{"fuse", 10, 229, 0o666},
		)
	}
	for _, d := range charDevs {
		p := filepath.Join(devDir, filepath.FromSlash(d.rel))
		if err := unix.Mknod(p, unix.S_IFCHR|d.mode, int(unix.Mkdev(d.major, d.minor))); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("mknod /dev/%s: %w", d.rel, err)
		}
		_ = unix.Chmod(p, d.mode)
	}
	for _, sl := range [][2]string{
		{"fd", "/proc/self/fd"},
		{"stdin", "/proc/self/fd/0"},
		{"stdout", "/proc/self/fd/1"},
		{"stderr", "/proc/self/fd/2"},
	} {
		_ = os.Symlink(sl[1], filepath.Join(devDir, sl[0]))
	}

	// Build /etc/{hostname,hosts,resolv.conf} in /run/gvx-ctr and bind-mount
	// them unless explicitly overridden by a user mount.
	userTargets := make(map[string]bool, len(spec.Mounts))
	for _, m := range spec.Mounts {
		userTargets[path.Clean(m.Target)] = true
	}

	if err := os.MkdirAll(ctrRunDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", ctrRunDir, err)
	}

	resolvContent, gatewayIP := waitForNetworkConfig(1500 * time.Millisecond)
	hostname := spec.Hostname
	if hostname == "" {
		hostname = "localhost"
	}

	etcFiles := []struct {
		target  string
		srcName string
		content string
	}{
		{"/etc/hostname", "hostname", hostname + "\n"},
		{"/etc/hosts", "hosts", renderHostsFile(hostname, gatewayIP, spec.ExtraHosts)},
		{"/etc/resolv.conf", "resolv.conf", resolvContent},
	}
	for _, ef := range etcFiles {
		if userTargets[ef.target] {
			continue
		}
		src := filepath.Join(ctrRunDir, ef.srcName)
		if err := os.WriteFile(src, []byte(ef.content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", src, err)
		}
		dst, err := resolveRootfsPath(ctrRootfs, ef.target, true)
		if err != nil {
			continue
		}
		if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
			if spec.ReadonlyRootfs {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				continue
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_RDONLY, 0o644)
			if err != nil {
				continue
			}
			_ = f.Close()
		}
		if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			// Fallback: on RW rootfs, write directly if bind mount fails.
			if !spec.ReadonlyRootfs {
				_ = os.WriteFile(dst, []byte(ef.content), 0o644)
			}
			continue
		}
		c.recordInnerMount(dst)
	}

	// Apply user mounts (bind, volume, tmpfs).
	volIdx := 0
	for i, m := range spec.Mounts {
		target, err := resolveRootfsPath(ctrRootfs, m.Target, true)
		if err != nil {
			return fmt.Errorf("mount %d target %q: %w", i, m.Target, err)
		}

		switch m.Kind {
		case "bind":
			fi, err := os.Stat(m.Source)
			if err != nil {
				return fmt.Errorf("bind source %s: %w", m.Source, err)
			}
			if fi.IsDir() {
				if err := ensureDir(target, spec.ReadonlyRootfs); err != nil {
					return fmt.Errorf("bind target dir %s: %w", m.Target, err)
				}
			} else {
				if err := ensureFile(target, spec.ReadonlyRootfs); err != nil {
					return fmt.Errorf("bind target file %s: %w", m.Target, err)
				}
			}
			if err := unix.Mount(m.Source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
				return fmt.Errorf("bind mount %s -> %s: %w", m.Source, m.Target, err)
			}
			if m.ReadOnly {
				if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
					return fmt.Errorf("remount ro %s: %w", m.Target, err)
				}
			}
			c.recordInnerMount(target)

		case "volume":
			volMount := fmt.Sprintf("%s%d", ctrVolBase, volIdx)
			volIdx++
			if err := mountVolumeDevice(m.Source, volMount, m.ReadOnly); err != nil {
				return fmt.Errorf("volume mount %s -> %s: %w", m.Source, m.Target, err)
			}
			c.mu.Lock()
			c.volMounts = append(c.volMounts, volMount)
			c.mu.Unlock()

			if err := ensureDir(target, spec.ReadonlyRootfs); err != nil {
				return fmt.Errorf("volume target dir %s: %w", m.Target, err)
			}
			if err := unix.Mount(volMount, target, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("bind volume %s -> %s: %w", volMount, m.Target, err)
			}
			if m.ReadOnly {
				if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
					return fmt.Errorf("remount volume ro %s: %w", m.Target, err)
				}
			}
			c.recordInnerMount(target)

		case "tmpfs":
			if err := ensureDir(target, spec.ReadonlyRootfs); err != nil {
				return fmt.Errorf("tmpfs target dir %s: %w", m.Target, err)
			}
			flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
			if m.ReadOnly {
				flags |= unix.MS_RDONLY
			}
			if err := unix.Mount("tmpfs", target, "tmpfs", flags, m.Options); err != nil {
				return fmt.Errorf("tmpfs mount %s: %w", m.Target, err)
			}
			c.recordInnerMount(target)
		}
	}

	return nil
}

func ensureDir(path string, readonly bool) error {
	if fi, err := os.Stat(path); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", path)
		}
		return nil
	}
	if readonly {
		return fmt.Errorf("%s does not exist on read-only rootfs", path)
	}
	return os.MkdirAll(path, 0o755)
}

func ensureFile(path string, readonly bool) error {
	if fi, err := os.Stat(path); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s exists and is a directory", path)
		}
		return nil
	}
	if readonly {
		return fmt.Errorf("%s does not exist on read-only rootfs", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func mountVolumeDevice(dev, mountPoint string, readOnly bool) error {
	if err := waitForPath(dev, 2*time.Second); err != nil {
		return fmt.Errorf("volume device %s not found: %w", dev, err)
	}
	fsType, err := probeFS(dev)
	if err != nil {
		return err
	}
	if fsType == "" {
		if readOnly {
			return fmt.Errorf("volume %s has no filesystem and is mounted read-only", dev)
		}
		out, err := exec.Command(
			"/sbin/mke2fs",
			"-t", "ext4",
			"-E", "lazy_itable_init=1,lazy_journal_init=1",
			"-q", "-F",
			dev,
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mke2fs %s: %v: %s", dev, err, strings.TrimSpace(string(out)))
		}
		fsType = "ext4"
	} else if !readOnly && (fsType == "ext4" || fsType == "ext3" || fsType == "ext2") {
		out, err := exec.Command("/sbin/e2fsck", "-p", dev).CombinedOutput()
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() >= 4 {
				return fmt.Errorf("e2fsck %s: %v: %s", dev, err, strings.TrimSpace(string(out)))
			}
		}
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPoint, err)
	}
	var flags uintptr
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(dev, mountPoint, fsType, flags, ""); err != nil {
		return fmt.Errorf("mount %s on %s (%s): %w", dev, mountPoint, fsType, err)
	}
	return nil
}

// waitForNetworkConfig waits up to timeout for udhcpc to populate
// /etc/resolv.conf and the default route when eth0 is present.
func waitForNetworkConfig(timeout time.Duration) (resolvConf, gatewayIP string) {
	const fallbackResolv = "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"

	// Skip waiting when networking is disabled (--network=none -> no eth0).
	if _, err := net.InterfaceByName("eth0"); err != nil {
		return fallbackResolv, ""
	}

	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile("/etc/resolv.conf"); err == nil && len(bytes.TrimSpace(b)) > 0 {
			resolvConf = string(b)
		}
		if b, err := os.ReadFile("/proc/net/route"); err == nil {
			gatewayIP = parseDefaultGateway(string(b))
		}
		if resolvConf != "" && gatewayIP != "" {
			return resolvConf, gatewayIP
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if resolvConf == "" {
		resolvConf = fallbackResolv
	}
	return resolvConf, gatewayIP
}

func (c *linuxContainerOps) setupCgroup(spec guestproto.ContainerSpec) error {
	// Enable controllers in root cgroup so child cgroup gets memory/cpu/pids files.
	if b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		for _, ctrl := range strings.Fields(string(b)) {
			switch ctrl {
			case "cpu", "memory", "pids", "io":
				_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+"+ctrl), 0o644)
			}
		}
	}
	if err := os.MkdirAll(ctrCgroupDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cgroup %s: %w", ctrCgroupDir, err)
	}
	c.mu.Lock()
	c.cgroupActive = true
	c.mu.Unlock()

	if spec.MemoryMax > 0 {
		_ = os.WriteFile(filepath.Join(ctrCgroupDir, "memory.max"), []byte(strconv.FormatInt(spec.MemoryMax, 10)), 0o644)
	}
	if spec.CPUQuota > 0 {
		period := spec.CPUPeriod
		if period <= 0 {
			period = 100000
		}
		_ = os.WriteFile(filepath.Join(ctrCgroupDir, "cpu.max"), []byte(fmt.Sprintf("%d %d", spec.CPUQuota, period)), 0o644)
	}
	if spec.PidsLimit > 0 {
		_ = os.WriteFile(filepath.Join(ctrCgroupDir, "pids.max"), []byte(strconv.FormatInt(spec.PidsLimit, 10)), 0o644)
	}
	return nil
}

func waitForPath(p string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return os.ErrNotExist
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// openPTY allocates a Linux /dev/ptmx + /dev/pts/N pair and sets initial size.
func openPTY(rows, cols uint16) (master *os.File, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	num, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", num)
	sfd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(mfd)
		return nil, nil, fmt.Errorf("open %s: %w", slavePath, err)
	}
	if rows > 0 || cols > 0 {
		_ = setPTYSize(mfd, rows, cols)
	}
	return os.NewFile(uintptr(mfd), "/dev/ptmx"), os.NewFile(uintptr(sfd), slavePath), nil
}

func setPTYSize(fd int, rows, cols uint16) error {
	ws := &unix.Winsize{Row: rows, Col: cols}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCSWINSZ), uintptr(unsafe.Pointer(ws)))
	if errno != 0 {
		return errno
	}
	return nil
}

// setPTYFileSize safely invokes TIOCSWINSZ via SyscallConn().Control so Go's
// poll.FD reference count prevents concurrent Close() from reusing the fd.
func setPTYFileSize(f *os.File, rows, cols uint16) error {
	if f == nil {
		return nil
	}
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := sc.Control(func(fd uintptr) {
		ioctlErr = setPTYSize(int(fd), rows, cols)
	}); err != nil {
		return err
	}
	return ioctlErr
}

func (c *linuxContainerOps) Attach() (guestproto.ContainerState, *outputHub, func([]byte) error, func() error, func(rows, cols uint16) error, error) {
	c.mu.Lock()
	if !c.started || c.hub == nil {
		c.mu.Unlock()
		return guestproto.ContainerState{}, nil, nil, nil, nil, errWithCode(guestproto.CodeNotRunning, errors.New("container has not been started"))
	}
	st := c.stateLocked()
	hub := c.hub
	c.mu.Unlock()

	writeStdin := func(p []byte) error {
		c.stdinMu.Lock()
		defer c.stdinMu.Unlock()
		if c.stdinClosed {
			return io.ErrClosedPipe
		}
		c.mu.Lock()
		pm := c.ptyMaster
		sp := c.stdinPipe
		c.mu.Unlock()
		if pm != nil {
			_, err := pm.Write(p)
			return err
		}
		if sp != nil {
			_, err := sp.Write(p)
			return err
		}
		return nil
	}
	closeStdin := func() error {
		c.stdinMu.Lock()
		defer c.stdinMu.Unlock()
		c.mu.Lock()
		sp := c.stdinPipe
		c.stdinPipe = nil
		c.mu.Unlock()
		if sp != nil {
			return sp.Close()
		}
		return nil
	}
	return st, hub, writeStdin, closeStdin, c.Resize, nil
}

func (c *linuxContainerOps) Signal(req guestproto.ContainerSignalRequest) error {
	sigNum, _, err := parseSignalName(req.Signal)
	if err != nil {
		return errWithCode(guestproto.CodeBadRequest, err)
	}
	sig := syscall.Signal(sigNum)

	c.mu.Lock()
	if !c.running || c.pid1 <= 0 {
		c.mu.Unlock()
		return errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}
	pid1 := c.pid1
	paused := c.paused
	c.mu.Unlock()

	if sig == unix.SIGKILL && paused {
		_ = os.WriteFile(filepath.Join(ctrCgroupDir, "cgroup.freeze"), []byte("0"), 0o644)
	}

	if req.All {
		pids := readCgroupPIDs(ctrCgroupDir)
		if len(pids) == 0 {
			pids = []int{pid1}
		}
		var firstErr error
		for _, p := range pids {
			if err := unix.Kill(p, sig); err != nil && !errors.Is(err, unix.ESRCH) && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	target := pid1
	if req.PID > 0 {
		if !isPIDInContainer(req.PID, pid1, ctrCgroupDir) {
			return errWithCode(guestproto.CodeNotFound, fmt.Errorf("pid %d is not in container", req.PID))
		}
		target = req.PID
	}
	if err := unix.Kill(target, sig); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errWithCode(guestproto.CodeNotRunning, err)
		}
		return err
	}
	return nil
}

func isPIDInContainer(pid, pid1 int, cgDir string) bool {
	if pid == pid1 {
		return true
	}
	for _, p := range readCgroupPIDs(cgDir) {
		if p == pid {
			return true
		}
	}
	return false
}

func readCgroupPIDs(cgDir string) []int {
	b, err := os.ReadFile(filepath.Join(cgDir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil && p > 0 {
			out = append(out, p)
		}
	}
	return out
}

func (c *linuxContainerOps) Resize(rows, cols uint16) error {
	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()
	c.mu.Lock()
	pm := c.ptyMaster
	running := c.running
	c.mu.Unlock()
	if !running || c.stdinClosed {
		return errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}
	if pm == nil {
		return errWithCode(guestproto.CodeBadRequest, errors.New("container has no TTY"))
	}
	return setPTYFileSize(pm, rows, cols)
}

type linuxExecProc struct {
	cmd       *exec.Cmd
	start     time.Time
	ptyMaster *os.File
	stdoutR   *os.File
	stderrR   *os.File
	stdinW    io.WriteCloser
	stdinMu   sync.Mutex
}

func (p *linuxExecProc) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *linuxExecProc) WriteStdin(data []byte) error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.ptyMaster != nil {
		_, err := p.ptyMaster.Write(data)
		return err
	}
	if p.stdinW != nil {
		_, err := p.stdinW.Write(data)
		return err
	}
	return nil
}

func (p *linuxExecProc) CloseStdin() error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdinW != nil {
		err := p.stdinW.Close()
		p.stdinW = nil
		return err
	}
	return nil
}

func (p *linuxExecProc) Resize(rows, cols uint16) error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.ptyMaster == nil {
		return nil
	}
	return setPTYFileSize(p.ptyMaster, rows, cols)
}

func (p *linuxExecProc) StreamOutput(writeFrame func(kind byte, data []byte) error) {
	p.stdinMu.Lock()
	pm := p.ptyMaster
	outR := p.stdoutR
	errR := p.stderrR
	p.stdinMu.Unlock()

	if pm != nil {
		buf := make([]byte, 32*1024)
		for {
			n, err := pm.Read(buf)
			if n > 0 {
				if writeFrame(guestproto.FrameStdout, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	var wg sync.WaitGroup
	pump := func(r *os.File, kind byte) {
		defer wg.Done()
		if r == nil {
			return
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if writeFrame(kind, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR, guestproto.FrameStdout)
	go pump(errR, guestproto.FrameStderr)
	wg.Wait()
}

func (p *linuxExecProc) Wait() guestproto.Exit {
	err := p.cmd.Wait()
	elapsed := time.Since(p.start)
	code, sigName, errMsg := extractExitInfo(p.cmd.ProcessState, err)
	return guestproto.Exit{
		Code:    code,
		Elapsed: elapsed.Nanoseconds(),
		Error:   errMsg,
		Signal:  sigName,
	}
}

func (p *linuxExecProc) Close() {
	p.stdinMu.Lock()
	sw := p.stdinW
	p.stdinW = nil
	pm := p.ptyMaster
	p.ptyMaster = nil
	outR := p.stdoutR
	p.stdoutR = nil
	errR := p.stderrR
	p.stderrR = nil
	p.stdinMu.Unlock()

	if sw != nil {
		_ = sw.Close()
	}
	for _, f := range []*os.File{pm, outR, errR} {
		if f != nil {
			_ = f.Close()
		}
	}
}

func (c *linuxContainerOps) Exec(req guestproto.ContainerExecRequest) (containerExecProc, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return nil, errWithCode(guestproto.CodeBadRequest, errors.New("exec requires argv"))
	}

	c.mu.Lock()
	if !c.running || c.pid1 <= 0 {
		c.mu.Unlock()
		return nil, errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}
	if c.paused {
		c.mu.Unlock()
		return nil, errWithCode(guestproto.CodePaused, errors.New("container is paused"))
	}
	pid1 := c.pid1
	spec := c.spec
	rootDir := c.rootDirLocked()
	c.mu.Unlock()

	userSpec := req.User
	if userSpec == "" {
		userSpec = spec.User
	}
	u, err := resolveUser(rootDir, userSpec)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, err)
	}

	wd := req.Workdir
	if wd == "" {
		wd = spec.Workdir
	}
	if wd == "" {
		wd = "/"
	}
	resolvedHostWD, err := resolveRootfsPath(rootDir, wd, true)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, fmt.Errorf("workdir %q: %w", wd, err))
	}
	if fi, err := os.Stat(resolvedHostWD); err != nil || !fi.IsDir() {
		return nil, errWithCode(guestproto.CodeBadRequest, fmt.Errorf("workdir %q is not a directory", wd))
	}
	relWD, err := filepath.Rel(rootDir, resolvedHostWD)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, fmt.Errorf("workdir %q: %w", wd, err))
	}
	nsenterWD := fmt.Sprintf("/proc/%d/root", pid1)
	if relWD != "." {
		nsenterWD = filepath.Join(nsenterWD, relWD)
	}

	env := mergeEnv(spec.Env, req.Env, u.Home, req.TTY)

	cgFD, err := unix.Open(ctrCgroupDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open cgroup: %w", err)
	}
	defer unix.Close(cgFD)

	var cmd *exec.Cmd
	sysAttr := &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    cgFD,
	}

	if _, err := os.Stat("/bin/busybox"); err == nil {
		// Enter the container's MNT, UTS, IPC, and PID namespaces via busybox
		// nsenter (single-threaded C binary, avoiding Go's multithreaded setns
		// EINVAL). Note: busybox getopt uses w:: (optional arg), so -w<dir>
		// must have NO space between -w and the path!
		args := []string{
			"nsenter",
			"-t", strconv.Itoa(pid1),
			"-m", "-u", "-i", "-p",
			"-r",
			"-w" + nsenterWD,
		}
		if u.UID != 0 || u.GID != 0 {
			args = append(args, "-S", strconv.Itoa(int(u.UID)), "-G", strconv.Itoa(int(u.GID)))
		}
		args = append(args, "--")
		args = append(args, req.Argv...)
		cmd = exec.Command("/bin/busybox", args...)
	} else {
		bin := resolveContainerExecPath(ctrRootfs, req.Argv[0], env)
		cmd = exec.Command(bin, req.Argv[1:]...)
		cmd.Dir = wd
		sysAttr.Chroot = ctrRootfs
		sysAttr.Credential = u.credential()
	}
	cmd.Env = env

	var (
		ptyMaster *os.File
		ptySlave  *os.File
		stdoutR   *os.File
		stdoutW   *os.File
		stderrR   *os.File
		stderrW   *os.File
		stdinR    *os.File
		stdinW    *os.File
	)
	closeAll := func() {
		for _, f := range []*os.File{ptyMaster, ptySlave, stdoutR, stdoutW, stderrR, stderrW, stdinR, stdinW} {
			if f != nil {
				_ = f.Close()
			}
		}
	}

	if req.TTY {
		ptyMaster, ptySlave, err = openPTY(req.Rows, req.Cols)
		if err != nil {
			return nil, fmt.Errorf("open exec pty: %w", err)
		}
		if u.UID != 0 || u.GID != 0 {
			_ = unix.Fchown(int(ptySlave.Fd()), int(u.UID), int(u.GID))
		}
		cmd.Stdin = ptySlave
		cmd.Stdout = ptySlave
		cmd.Stderr = ptySlave
		sysAttr.Setsid = true
		sysAttr.Setctty = true
		sysAttr.Ctty = 0
	} else {
		stdoutR, stdoutW, err = os.Pipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			closeAll()
			return nil, err
		}
		cmd.Stdout = stdoutW
		cmd.Stderr = stderrW
		if req.Interactive {
			stdinR, stdinW, err = os.Pipe()
			if err != nil {
				closeAll()
				return nil, err
			}
			cmd.Stdin = stdinR
		}
	}
	cmd.SysProcAttr = sysAttr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}

	if ptySlave != nil {
		_ = ptySlave.Close()
	}
	if stdoutW != nil {
		_ = stdoutW.Close()
	}
	if stderrW != nil {
		_ = stderrW.Close()
	}
	if stdinR != nil {
		_ = stdinR.Close()
	}

	return &linuxExecProc{
		cmd:       cmd,
		start:     start,
		ptyMaster: ptyMaster,
		stdoutR:   stdoutR,
		stderrR:   stderrR,
		stdinW:    stdinW,
	}, nil
}

func resolveContainerExecPath(rootfs, bin string, env []string) string {
	if strings.ContainsRune(bin, '/') {
		return bin
	}
	pathVal := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			pathVal = v
			break
		}
	}
	for _, dir := range strings.Split(pathVal, ":") {
		if dir == "" {
			continue
		}
		cand := path.Join(dir, bin)
		hostCand, err := resolveRootfsPath(rootfs, cand, true)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(hostCand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand
		}
	}
	return bin
}

func (c *linuxContainerOps) Top() ([]guestproto.ContainerProcess, error) {
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if !running {
		return nil, errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}

	pids := readCgroupPIDs(ctrCgroupDir)
	pageSize := uint64(os.Getpagesize())
	procs := make([]guestproto.ContainerProcess, 0, len(pids))
	for _, pid := range pids {
		pdir := fmt.Sprintf("/proc/%d", pid)
		statB, err := os.ReadFile(filepath.Join(pdir, "stat"))
		if err != nil {
			continue
		}
		statusB, _ := os.ReadFile(filepath.Join(pdir, "status"))
		cmdB, _ := os.ReadFile(filepath.Join(pdir, "cmdline"))
		if proc, ok := parseProcProcess(int32(pid), string(statB), string(statusB), cmdB, 100, pageSize); ok {
			procs = append(procs, proc)
		}
	}
	return procs, nil
}

func (c *linuxContainerOps) Stats() (guestproto.ContainerStats, error) {
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if !running {
		return guestproto.ContainerStats{}, errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}

	readStr := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}
	st := parseCgroupStats(
		readStr(filepath.Join(ctrCgroupDir, "cpu.stat")),
		readStr(filepath.Join(ctrCgroupDir, "memory.current")),
		readStr(filepath.Join(ctrCgroupDir, "memory.stat")),
		readStr(filepath.Join(ctrCgroupDir, "memory.max")),
		readStr("/proc/meminfo"),
		readStr(filepath.Join(ctrCgroupDir, "pids.current")),
		readStr(filepath.Join(ctrCgroupDir, "pids.max")),
		readStr("/proc/net/dev"),
	)
	return st, nil
}

func (c *linuxContainerOps) Pause() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}
	if c.paused {
		c.mu.Unlock()
		return errWithCode(guestproto.CodeAlreadyPaused, errors.New("container is already paused"))
	}
	c.mu.Unlock()

	if err := os.WriteFile(filepath.Join(ctrCgroupDir, "cgroup.freeze"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("freeze cgroup: %w", err)
	}
	waitCgroupFrozen(ctrCgroupDir, true, 2*time.Second)

	c.mu.Lock()
	c.paused = true
	c.mu.Unlock()
	return nil
}

func (c *linuxContainerOps) Unpause() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return errWithCode(guestproto.CodeNotRunning, errors.New("container is not running"))
	}
	if !c.paused {
		c.mu.Unlock()
		return errWithCode(guestproto.CodeNotPaused, errors.New("container is not paused"))
	}
	c.mu.Unlock()

	if err := os.WriteFile(filepath.Join(ctrCgroupDir, "cgroup.freeze"), []byte("0"), 0o644); err != nil {
		return fmt.Errorf("thaw cgroup: %w", err)
	}
	waitCgroupFrozen(ctrCgroupDir, false, 2*time.Second)

	c.mu.Lock()
	c.paused = false
	c.mu.Unlock()
	return nil
}

func waitCgroupFrozen(cgDir string, wantFrozen bool, timeout time.Duration) {
	want := "frozen 0"
	if wantFrozen {
		want = "frozen 1"
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(cgDir, "cgroup.events"))
		if err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *linuxContainerOps) Stat(req guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, error) {
	if strings.TrimSpace(req.Path) == "" {
		return guestproto.ContainerPathStat{}, errWithCode(guestproto.CodeBadRequest, errors.New("path is required"))
	}
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return guestproto.ContainerPathStat{}, errWithCode(guestproto.CodeNotRunning, errors.New("container not started"))
	}
	root := c.rootDirLocked()
	c.mu.Unlock()

	st, _, err := statContainerPath(root, req.Path, req.FollowLink)
	return st, err
}

func (c *linuxContainerOps) CopyFrom(req guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, func(io.Writer) error, error) {
	if strings.TrimSpace(req.Path) == "" {
		return guestproto.ContainerPathStat{}, nil, errWithCode(guestproto.CodeBadRequest, errors.New("path is required"))
	}
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return guestproto.ContainerPathStat{}, nil, errWithCode(guestproto.CodeNotRunning, errors.New("container not started"))
	}
	root := c.rootDirLocked()
	c.mu.Unlock()

	st, hostPath, err := statContainerPath(root, req.Path, req.FollowLink)
	if err != nil {
		return guestproto.ContainerPathStat{}, nil, err
	}
	clean := path.Clean("/" + req.Path)
	includeBase := clean != "/"
	stream := func(w io.Writer) error {
		return writeTarFromPath(hostPath, includeBase, false, clean == "/", w)
	}
	return st, stream, nil
}

func (c *linuxContainerOps) CopyTo(req guestproto.ContainerCopyRequest) (func(io.Reader) error, error) {
	if strings.TrimSpace(req.Path) == "" {
		return nil, errWithCode(guestproto.CodeBadRequest, errors.New("path is required"))
	}
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil, errWithCode(guestproto.CodeNotRunning, errors.New("container not started"))
	}
	root := c.rootDirLocked()
	c.mu.Unlock()

	dest, err := resolveRootfsPath(root, req.Path, true)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errWithCode(guestproto.CodeNotFound, err)
		}
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errWithCode(guestproto.CodeBadRequest, fmt.Errorf("destination %q is not a directory", req.Path))
	}
	extract := func(r io.Reader) error {
		return extractTarToDir(root, dest, req.NoOverwriteDir, r)
	}
	return extract, nil
}

func (c *linuxContainerOps) Export(req guestproto.ContainerExportRequest) (func(io.Writer) error, error) {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil, errWithCode(guestproto.CodeNotRunning, errors.New("container not started"))
	}
	root := c.rootDirLocked()
	roRootfs := c.spec.ReadonlyRootfs
	c.mu.Unlock()

	sub := req.Path
	if sub == "" {
		sub = "/"
	}
	if req.UpperOnly {
		if roRootfs {
			return func(w io.Writer) error {
				return writeTarFromPath(filepath.Join(ctrUpperDir, ".nonexistent-ro"), false, true, false, w)
			}, nil
		}
		hostPath, err := resolveRootfsPath(ctrUpperDir, sub, false)
		if err != nil {
			return nil, errWithCode(guestproto.CodeBadRequest, err)
		}
		return func(w io.Writer) error {
			return writeTarFromPath(hostPath, false, true, false, w)
		}, nil
	}

	hostPath, err := resolveRootfsPath(root, sub, true)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, err)
	}
	if _, err := os.Lstat(hostPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errWithCode(guestproto.CodeNotFound, err)
		}
		return nil, err
	}
	clean := path.Clean("/" + sub)
	return func(w io.Writer) error {
		return writeTarFromPath(hostPath, false, false, clean == "/", w)
	}, nil
}

func (c *linuxContainerOps) Shutdown(timeout time.Duration) error {
	c.mu.Lock()
	started := c.started
	running := c.running
	paused := c.paused
	pid1 := c.pid1
	done := c.done
	c.mu.Unlock()

	if !started {
		return nil
	}

	if running && pid1 > 0 {
		if paused {
			_ = os.WriteFile(filepath.Join(ctrCgroupDir, "cgroup.freeze"), []byte("0"), 0o644)
		}
		_ = unix.Kill(pid1, unix.SIGTERM)
		timer := time.NewTimer(timeout)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			for _, p := range readCgroupPIDs(ctrCgroupDir) {
				_ = unix.Kill(p, unix.SIGKILL)
			}
			_ = unix.Kill(pid1, unix.SIGKILL)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	}

	c.cleanupResources()
	return nil
}

func (c *linuxContainerOps) cleanupResources() {
	c.mu.Lock()
	inner := append([]string(nil), c.innerMounts...)
	vols := append([]string(nil), c.volMounts...)
	layers := append([]string(nil), c.layerMounts...)
	loops := append([]string(nil), c.loopDevs...)
	rootfsUp := c.rootfsUp
	cgActive := c.cgroupActive
	c.innerMounts = nil
	c.volMounts = nil
	c.layerMounts = nil
	c.loopDevs = nil
	c.rootfsUp = false
	c.cgroupActive = false
	c.mu.Unlock()

	// Kill any stragglers still in the container cgroup.
	if cgActive {
		for _, p := range readCgroupPIDs(ctrCgroupDir) {
			_ = unix.Kill(p, unix.SIGKILL)
		}
		_ = os.Remove(ctrCgroupDir)
	}

	// Unmount inner mounts in reverse order (binds, volume binds, tmpfs, /dev/*).
	for i := len(inner) - 1; i >= 0; i-- {
		unmountWithFallback(inner[i], false)
	}

	if rootfsUp {
		unmountWithFallback(ctrRootfs, false)
	}

	// Sync and cleanly unmount ext4 volume mounts so journals flush.
	unix.Sync()
	for i := len(vols) - 1; i >= 0; i-- {
		unmountWithFallback(vols[i], true)
	}

	// Unmount EROFS layer mounts and detach loop devices.
	for i := len(layers) - 1; i >= 0; i-- {
		unmountWithFallback(layers[i], false)
	}
	for i := len(loops) - 1; i >= 0; i-- {
		if fd, err := unix.Open(loops[i], unix.O_RDONLY|unix.O_CLOEXEC, 0); err == nil {
			_, _ = unix.IoctlRetInt(fd, unix.LOOP_CLR_FD)
			_ = unix.Close(fd)
		}
	}
	_ = os.RemoveAll(ctrRunDir)
}

func unmountWithFallback(target string, syncExt4 bool) {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		err := unix.Unmount(target, 0)
		if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return
		}
		if !errors.Is(err, unix.EBUSY) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if syncExt4 {
		if err := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err == nil {
			unix.Sync()
			return
		}
	}
	if err := unix.Unmount(target, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		log.Printf("umount %s: %v", target, err)
	}
}

// isOverlayWhiteout reports whether fi is a Linux overlayfs whiteout
// (character device with rdev == 0:0).
func isOverlayWhiteout(fi fs.FileInfo) bool {
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Rdev == 0
}

// isOverlayOpaqueDir reports whether dirPath carries overlayfs's
// trusted.overlay.opaque="y" xattr.
func isOverlayOpaqueDir(dirPath string) bool {
	var buf [8]byte
	n, err := unix.Lgetxattr(dirPath, "trusted.overlay.opaque", buf[:])
	return err == nil && n == 1 && buf[0] == 'y'
}

// runContainerInit is the PID 1 child entrypoint (`gvx-agent __ctr-init`).
// It isolates mounts, performs pivot_root, mounts /proc and /sys, sets the
// hostname, drops capabilities (when unprivileged) and credentials, chdirs to
// Workdir, and execs the container's entrypoint.
func runContainerInit() {
	// FD 3 is init config JSON; FD 4 is the CLOEXEC error pipe back to parent.
	unix.CloseOnExec(4)
	errPipe := os.NewFile(4, "init-err")
	failInit := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		_, _ = io.WriteString(errPipe, msg)
		_ = errPipe.Close()
		os.Exit(127)
	}

	cfgFile := os.NewFile(3, "init-cfg")
	cfgBytes, err := io.ReadAll(cfgFile)
	_ = cfgFile.Close()
	if err != nil {
		failInit("read init config: %v", err)
	}
	var cfg ctrInitConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		failInit("decode init config: %v", err)
	}

	// Make mount propagation private before pivot_root.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		failInit("make root private: %v", err)
	}

	// pivot_root(".", ".") works on both RW and ReadonlyRootfs without creating
	// a temporary directory inside the container rootfs.
	if err := unix.Chdir(ctrRootfs); err != nil {
		failInit("chdir %s: %v", ctrRootfs, err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		failInit("pivot_root: %v", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		failInit("umount oldroot: %v", err)
	}
	if err := unix.Chdir("/"); err != nil {
		failInit("chdir /: %v", err)
	}

	// Mount /proc for the new PID namespace and /sys (ro unless privileged).
	if !cfg.ReadonlyRootfs {
		_ = os.MkdirAll("/proc", 0o555)
		_ = os.MkdirAll("/sys", 0o555)
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		failInit("mount /proc: %v", err)
	}
	sysFlags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if !cfg.Privileged {
		sysFlags |= unix.MS_RDONLY
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", sysFlags, ""); err != nil {
		failInit("mount /sys: %v", err)
	}

	if cfg.Hostname != "" {
		if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
			failInit("sethostname %q: %v", cfg.Hostname, err)
		}
	}

	wd := cfg.Workdir
	if wd == "" {
		wd = "/"
	}
	if !cfg.ReadonlyRootfs {
		if _, err := os.Stat(wd); errors.Is(err, os.ErrNotExist) {
			_ = os.MkdirAll(wd, 0o755)
		}
	}

	// Drop non-default capabilities for unprivileged containers before dropping UID.
	if !cfg.Privileged {
		if err := dropUnprivilegedCaps(cfg.User.UID == 0); err != nil {
			failInit("drop capabilities: %v", err)
		}
	}

	if err := cfg.User.applyCurrentProcess(); err != nil {
		failInit("apply user %d:%d: %v", cfg.User.UID, cfg.User.GID, err)
	}

	if err := unix.Chdir(wd); err != nil {
		failInit("chdir %q: %v", wd, err)
	}

	binPath, err := lookupPathInContainer(cfg.Argv[0], cfg.Env)
	if err != nil {
		failInit("%v", err)
	}

	// Exec replaces this process; FD 4 (CLOEXEC) closes automatically on success.
	if err := unix.Exec(binPath, cfg.Argv, cfg.Env); err != nil {
		failInit("exec %s: %v", binPath, err)
	}
}

func lookupPathInContainer(cmd string, env []string) (string, error) {
	if strings.ContainsRune(cmd, '/') {
		if _, err := os.Stat(cmd); err != nil {
			return "", fmt.Errorf("executable %q: %w", cmd, err)
		}
		return cmd, nil
	}
	pathEnv := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			pathEnv = v
			break
		}
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, cmd)
		fi, err := os.Stat(cand)
		if err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in $PATH", cmd)
}

// dockerDefaultCaps matches Docker's standard unprivileged capability set.
var dockerDefaultCaps = map[int]bool{
	unix.CAP_CHOWN:            true,
	unix.CAP_DAC_OVERRIDE:     true,
	unix.CAP_FSETID:           true,
	unix.CAP_FOWNER:           true,
	unix.CAP_MKNOD:            true,
	unix.CAP_NET_RAW:          true,
	unix.CAP_SETGID:           true,
	unix.CAP_SETUID:           true,
	unix.CAP_SETFCAP:          true,
	unix.CAP_SETPCAP:          true,
	unix.CAP_NET_BIND_SERVICE: true,
	unix.CAP_SYS_CHROOT:       true,
	unix.CAP_KILL:             true,
	unix.CAP_AUDIT_WRITE:      true,
}

func dropUnprivilegedCaps(keepDefaultForRoot bool) error {
	lastCap := unix.CAP_LAST_CAP
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
			lastCap = n
		}
	}
	for capNum := 0; capNum <= lastCap; capNum++ {
		if dockerDefaultCaps[capNum] {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capNum), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("PR_CAPBSET_DROP %d: %w", capNum, err)
		}
	}

	if !keepDefaultForRoot {
		// setuid(non-zero) will clear effective/permitted caps; bounding set is already narrowed.
		return nil
	}

	var lo, hi uint32
	for capNum := range dockerDefaultCaps {
		if capNum < 32 {
			lo |= 1 << uint(capNum)
		} else if capNum < 64 {
			hi |= 1 << uint(capNum-32)
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: lo, Permitted: lo, Inheritable: 0},
		{Effective: hi, Permitted: hi, Inheritable: 0},
	}
	return unix.Capset(&hdr, &data[0])
}
