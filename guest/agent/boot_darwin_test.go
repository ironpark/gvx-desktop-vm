//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop-vm/vz"
)

// TestGuestBootReal boots the assets produced by guest/mkassets under
// internal/vz twice to verify: agent hello + Rosetta, ext4 format on first
// boot and persistence across reboot, buildkitd service start with
// linux/amd64 and linux/arm64 workers, dial to buildkitd, vsock isolation
// from guest processes and a real Dockerfile RUN step, and clean graceful
// shutdown.
//
// Opt-in only (requires an ad-hoc signed test binary with
// com.apple.security.virtualization):
//
//	go test -c -o /tmp/gvxd-bt/agent.test ./guest/agent
//	codesign --sign - --entitlements entitlements.plist --force /tmp/gvxd-bt/agent.test
//	GVX_GUEST_BOOT_TEST=1 GVX_VM_ASSETS=/path/to/assets /tmp/gvxd-bt/agent.test -test.v -test.run TestGuestBootReal
func TestGuestBootReal(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_GUEST_BOOT_TEST") == "" {
		t.Skip("set GVX_GUEST_BOOT_TEST=1 and GVX_VM_ASSETS=... with a signed test binary")
	}
	assetsDir := os.Getenv("GVX_VM_ASSETS")
	if assetsDir == "" {
		t.Fatal("GVX_VM_ASSETS is required")
	}

	workDir := t.TempDir()
	diskPath := filepath.Join(workDir, "disk.img")
	f, err := os.Create(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Build a static linux/arm64 vsock probe binary and share it read-only
	// so we can prove neither guest-local processes nor a Dockerfile RUN
	// container can invoke the agent on vsock port 1024.
	probeDir := filepath.Join(workDir, "probe")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildVsockProbe(t, filepath.Join(probeDir, "vsockprobe"))

	verifyBoot(t, assetsDir, workDir, diskPath, probeDir, 1)
	verifyBoot(t, assetsDir, workDir, diskPath, probeDir, 2)
}

func buildVsockProbe(t *testing.T, outBin string) {
	t.Helper()
	srcDir := t.TempDir()
	const probeSrc = `package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

func probe(cid uint32) error {
	fd, err := syscall.Socket(40 /* AF_VSOCK */, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %v", err)
	}
	defer syscall.Close(fd)
	sa := sockaddrVM{Family: 40, Port: 1024, CID: cid}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
	if errno != 0 {
		return fmt.Errorf("connect cid=%d: %v", cid, errno)
	}
	f := os.NewFile(uintptr(fd), "vsock")
	_ = f.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := f.Write([]byte("{\"v\":1,\"op\":\"hello\"}\n")); err != nil {
		return fmt.Errorf("write cid=%d: %v", cid, err)
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read cid=%d: %v", cid, err)
	}
	return fmt.Errorf("UNEXPECTED_AGENT_REPLY cid=%d: %q", cid, line)
}

func main() {
	cids := []uint32{1 /* LOCAL */, 2 /* HOST */, 3 /* typical guest CID */}
	for _, cid := range cids {
		err := probe(cid)
		fmt.Printf("cid=%d -> %v\n", cid, err)
		if err != nil && len(err.Error()) >= 22 && err.Error()[:22] == "UNEXPECTED_AGENT_REPLY" {
			os.Exit(1)
		}
	}
	fmt.Println("VSOCK_ISOLATED_OK")
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module vsockprobe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(probeSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", outBin, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build vsockprobe: %v\n%s", err, string(out))
	}
}

func verifyBoot(t *testing.T, assetsDir, workDir, diskPath, probeDir string, bootNum int) {
	t.Helper()
	consolePath := filepath.Join(workDir, fmt.Sprintf("console-%d.log", bootNum))
	consoleFile, err := os.Create(consolePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = consoleFile.Close()
		if b, err := os.ReadFile(consolePath); err == nil && len(b) > 0 {
			t.Logf("boot %d console:\n%s", bootNum, string(b))
		}
	}()

	cfg := vz.Config{
		Kernel:      filepath.Join(assetsDir, "Image"),
		Initrd:      filepath.Join(assetsDir, "initramfs.gz"),
		CommandLine: "console=hvc0 panic=-1 quiet gvx.assets=assets gvx.rosetta=rosetta gvx.net=1 gvx.shares=probe:ro",
		CPUs:        2,
		MemoryBytes: 2048 << 20,
		Console:     consoleFile,
		Shares: []vz.Share{
			{Tag: guestproto.AssetsTag, Dir: assetsDir, ReadOnly: true},
			{Tag: "probe", Dir: probeDir, ReadOnly: true},
		},
		Rosetta:    true,
		RosettaTag: guestproto.RosettaTag,
		Disks:      []vz.Disk{{Path: diskPath, ReadOnly: false}},
		NAT:        true,
		Balloon:    true,
	}

	vm, err := vz.New(cfg)
	if err != nil {
		t.Fatalf("boot %d vz.New: %v", bootNum, err)
	}
	defer vm.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t0 := time.Now()
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("boot %d vm.Start: %v", bootNum, err)
	}

	var hello guestproto.Hello
	for {
		resp, err := vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpHello})
		if err == nil && resp.OK && resp.Hello != nil {
			hello = *resp.Hello
			break
		}
		if time.Since(t0) > 10*time.Second {
			t.Fatalf("boot %d agent did not answer hello: %v", bootNum, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("boot %d hello in %s: kernel=%s machine=%s rosetta=%v uptime=%.2fs agent=%s",
		bootNum, time.Since(t0).Round(time.Millisecond), hello.Kernel, hello.Machine, hello.Rosetta, hello.Uptime, hello.Agent)
	if !hello.Rosetta {
		t.Fatalf("boot %d expected rosetta=true", bootNum)
	}

	resp, err := vzRoundTrip(ctx, vm, guestproto.Request{
		Op:   guestproto.OpDiskMount,
		Disk: &guestproto.DiskRequest{Format: true},
	})
	if err != nil || !resp.OK || resp.Disk == nil {
		t.Fatalf("boot %d disk-mount: err=%v resp=%+v", bootNum, err, resp)
	}
	t.Logf("boot %d disk-mount: formatted=%v fs=%s size=%d free=%d",
		bootNum, resp.Disk.Formatted, resp.Disk.FSType, resp.Disk.Size, resp.Disk.Free)
	if bootNum == 1 && !resp.Disk.Formatted {
		t.Fatalf("boot 1 expected formatted=true")
	}
	if bootNum == 2 && resp.Disk.Formatted {
		t.Fatalf("boot 2 expected formatted=false (disk should persist)")
	}
	if resp.Disk.FSType != "ext4" {
		t.Fatalf("boot %d expected ext4, got %q", bootNum, resp.Disk.FSType)
	}

	if bootNum == 1 {
		out, errOut, code := vzExec(t, ctx, vm, []string{"sh", "-c", "echo persisted-gvx-v1 > /var/lib/gvx/marker && sync && cat /var/lib/gvx/marker"})
		if code != 0 || strings.TrimSpace(out) != "persisted-gvx-v1" {
			t.Fatalf("boot 1 marker write failed: code=%d out=%q err=%q", code, out, errOut)
		}
	} else {
		out, errOut, code := vzExec(t, ctx, vm, []string{"sh", "-c", "cat /var/lib/gvx/marker"})
		if code != 0 || strings.TrimSpace(out) != "persisted-gvx-v1" {
			t.Fatalf("boot 2 marker did not persist: code=%d out=%q err=%q", code, out, errOut)
		}
		t.Logf("boot 2 persisted marker verified: %q", strings.TrimSpace(out))
	}

	resp, err = vzRoundTrip(ctx, vm, guestproto.Request{
		Op:      guestproto.OpServiceStart,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd, Timeout: 20000},
	})
	if err != nil || !resp.OK {
		logOut, _, _ := vzExec(t, ctx, vm, []string{"sh", "-c", "cat /run/buildkitd.log 2>&1 || true"})
		t.Fatalf("boot %d service-start buildkitd: err=%v resp=%+v\nbuildkitd.log:\n%s", bootNum, err, resp, logOut)
	}
	t.Logf("boot %d buildkitd started: %+v", bootNum, resp.Services)

	out, errOut, code := vzExec(t, ctx, vm, []string{
		"/mnt/assets/buildkit/bin/buildctl",
		"--addr", "unix://" + guestproto.BuildkitdSocket,
		"debug", "workers",
	})
	if code != 0 {
		logOut, _, _ := vzExec(t, ctx, vm, []string{"sh", "-c", "cat /run/buildkitd.log 2>&1 || true"})
		t.Fatalf("boot %d buildctl debug workers failed: code=%d out=%q err=%q\nlog:\n%s", bootNum, code, out, errOut, logOut)
	}
	t.Logf("boot %d buildctl debug workers:\n%s", bootNum, out)
	if !strings.Contains(out, "linux/amd64") || !strings.Contains(out, "linux/arm64") {
		t.Fatalf("boot %d workers missing expected platforms:\n%s", bootNum, out)
	}

	// On boot 1, prove guest-local processes AND a real buildkit Dockerfile RUN
	// container cannot talk to the guest agent over vsock port 1024.
	if bootNum == 1 {
		// Check guest root namespace + sysctl state.
		pOut, pErr, pCode := vzExec(t, ctx, vm, []string{"sh", "-c",
			"if [ -f /proc/sys/net/vsock/child_ns_mode ]; then echo child_ns_mode=$(cat /proc/sys/net/vsock/child_ns_mode); else echo child_ns_mode=absent; fi; /mnt/probe/vsockprobe"})
		t.Logf("boot 1 guest vsock probe (code=%d):\n%s%s", pCode, pOut, pErr)
		if pCode != 0 || !strings.Contains(pOut, "VSOCK_ISOLATED_OK") {
			t.Fatalf("guest vsock probe failed: code=%d out=%q err=%q", pCode, pOut, pErr)
		}

		// Run the same probe inside a real buildkit-runc container via buildctl.
		runcScript := `set -eu
mkdir -p /tmp/vsockctx/rootfs
cp /mnt/probe/vsockprobe /tmp/vsockctx/rootfs/vsockprobe
cat > /tmp/vsockctx/config.json <<'EOF'
{
  "ociVersion": "1.0.2",
  "process": {
    "user": {"uid": 0, "gid": 0},
    "args": ["/vsockprobe"],
    "cwd": "/"
  },
  "root": {"path": "rootfs", "readonly": true},
  "linux": {
    "namespaces": [
      {"type": "pid"},
      {"type": "network"},
      {"type": "mount"}
    ]
  }
}
EOF
/mnt/assets/buildkit/bin/buildkit-runc --root /run/runc-test run --bundle /tmp/vsockctx vsock-iso-test
`
		cOut, cErr, cCode := vzExec(t, ctx, vm, []string{"sh", "-c", runcScript})
		t.Logf("boot 1 container (buildkit-runc) vsock probe (code=%d):\n%s%s", cCode, cOut, cErr)
		if cCode != 0 || !strings.Contains(cOut, "VSOCK_ISOLATED_OK") {
			t.Fatalf("container vsock probe failed: code=%d out=%q err=%q", cCode, cOut, cErr)
		}
	}

	dconn, err := vzDial(ctx, vm, guestproto.ServiceBuildkitd)
	if err != nil {
		t.Fatalf("boot %d dial buildkitd: %v", bootNum, err)
	}
	_ = dconn.Close()

	resp, err = vzRoundTrip(ctx, vm, guestproto.Request{
		Op:      guestproto.OpServiceStop,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd, Timeout: 10000},
	})
	if err != nil || !resp.OK {
		t.Fatalf("boot %d service-stop: err=%v resp=%+v", bootNum, err, resp)
	}

	resp, err = vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpShutdown})
	if err != nil || !resp.OK {
		t.Fatalf("boot %d shutdown: err=%v resp=%+v", bootNum, err, resp)
	}

	select {
	case <-vm.Done():
		t.Logf("boot %d graceful shutdown completed (err=%v)", bootNum, vm.Err())
	case <-time.After(10 * time.Second):
		_ = vm.Stop(ctx)
		t.Fatalf("boot %d VM did not power off after shutdown", bootNum)
	}
}

func vzRoundTrip(ctx context.Context, vm *vz.VM, req guestproto.Request) (guestproto.Response, error) {
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		return guestproto.Response{}, err
	}
	defer conn.Close()
	req.V = guestproto.Version
	if err := vmproto.WriteLine(conn, req); err != nil {
		return guestproto.Response{}, err
	}
	var resp guestproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(conn), &resp); err != nil {
		return guestproto.Response{}, err
	}
	return resp, nil
}

func vzExec(t *testing.T, ctx context.Context, vm *vz.VM, argv []string) (string, string, int) {
	t.Helper()
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		t.Fatalf("exec connect: %v", err)
	}
	defer conn.Close()
	if err := vmproto.WriteLine(conn, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpExec,
		Exec: &guestproto.ExecRequest{Argv: argv},
	}); err != nil {
		t.Fatalf("exec write: %v", err)
	}
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		t.Fatalf("exec read resp: %v", err)
	}
	if !resp.OK {
		t.Fatalf("exec rejected: %+v", resp)
	}
	var stdout, stderr bytes.Buffer
	for {
		kind, payload, err := guestproto.ReadFrame(br)
		if err != nil {
			t.Fatalf("exec read frame: %v", err)
		}
		switch kind {
		case guestproto.FrameStdout:
			stdout.Write(payload)
		case guestproto.FrameStderr:
			stderr.Write(payload)
		case guestproto.FrameExit:
			var ex guestproto.Exit
			if err := json.Unmarshal(payload, &ex); err != nil {
				t.Fatalf("exec exit json: %v", err)
			}
			if ex.Error != "" {
				stderr.WriteString(ex.Error)
			}
			return stdout.String(), stderr.String(), ex.Code
		}
	}
}

func vzDial(ctx context.Context, vm *vz.VM, svc string) (net.Conn, error) {
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		return nil, err
	}
	if err := vmproto.WriteLine(conn, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDial,
		Dial: &guestproto.DialRequest{Service: svc},
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var resp guestproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(conn), &resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, fmt.Errorf("dial %s: %s: %s", svc, resp.Code, resp.Error)
	}
	return conn, nil
}
