//go:build darwin

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// TestContainerBootReal boots vm-assets-v2 with real EROFS layers and exercises
// the full micro-VM container runtime end-to-end (arm64 native fastpath,
// amd64 Rosetta + overflow loop.ko path, bind/volume/tmpfs mounts, pivot_root
// isolation, PTY + pipes, nsenter exec, top, stats, pause/unpause, stat,
// copy-from, copy-to, export upperOnly whiteouts, and clean shutdown).
//
// Opt-in only (signed test binary + GVX_GUEST_BOOT_TEST=1 + GVX_VM_ASSETS +
// GVX_TEST_EROFS_LAYERS=layer0.erofs,layer1.erofs):
func TestContainerBootReal(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_GUEST_BOOT_TEST") == "" {
		t.Skip("set GVX_GUEST_BOOT_TEST=1, GVX_VM_ASSETS, and GVX_TEST_EROFS_LAYERS")
	}
	assetsDir := os.Getenv("GVX_VM_ASSETS")
	layersEnv := os.Getenv("GVX_TEST_EROFS_LAYERS")
	if assetsDir == "" || layersEnv == "" {
		t.Skip("GVX_VM_ASSETS and GVX_TEST_EROFS_LAYERS are required")
	}
	layerFiles := strings.Split(layersEnv, ",")
	if len(layerFiles) < 2 {
		t.Fatalf("need at least 2 EROFS layers, got %v", layerFiles)
	}

	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bins")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildCtrTestBinary(t, filepath.Join(binDir, "ctrtest-arm64"), "arm64")
	buildCtrTestBinary(t, filepath.Join(binDir, "ctrtest-amd64"), "amd64")

	// Subtest 1: arm64 native (Rosetta OFF), fastpath virtio-blk EROFS layers,
	// ext4 volume, dir & single-file bind mounts, tmpfs, exec (nsenter),
	// top, stats, pause/unpause, stat, copy-from, copy-to, export (upperOnly).
	t.Run("arm64_native_fastpath_full_lifecycle", func(t *testing.T) {
		runDir := filepath.Join(workDir, "arm64")
		_ = os.MkdirAll(runDir, 0o755)
		rootfsDisk := makeSparseDisk(t, filepath.Join(runDir, "rootfs.ext4"), 256<<20)
		volDisk := makeSparseDisk(t, filepath.Join(runDir, "vol.ext4"), 128<<20)
		bindDir := filepath.Join(runDir, "bind")
		_ = os.MkdirAll(bindDir, 0o755)
		_ = os.WriteFile(filepath.Join(bindDir, "from-host.txt"), []byte("host-bind-ok\n"), 0o644)
		singleFile := filepath.Join(runDir, "single.txt")
		_ = os.WriteFile(singleFile, []byte("single-file-ok\n"), 0o644)
		singleDir := filepath.Join(runDir, "single-parent")
		_ = os.MkdirAll(singleDir, 0o755)
		_ = os.WriteFile(filepath.Join(singleDir, "single.txt"), []byte("single-file-ok\n"), 0o644)
		_ = singleFile

		consolePath := filepath.Join(runDir, "console.log")
		consoleFile, err := os.Create(consolePath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = consoleFile.Close()
			if b, err := os.ReadFile(consolePath); err == nil && len(b) > 0 {
				t.Logf("arm64 console:\n%s", string(b))
			}
		}()

		cfg := vz.Config{
			Kernel:      filepath.Join(assetsDir, "Image"),
			Initrd:      filepath.Join(assetsDir, "initramfs.gz"),
			CommandLine: "console=hvc0 panic=-1 quiet gvx.net=1 gvx.shares=bins:ro,b0,b1:ro",
			CPUs:        2,
			MemoryBytes: 512 << 20,
			Console:     consoleFile,
			Shares: []vz.Share{
				{Tag: "bins", Dir: binDir, ReadOnly: true},
				{Tag: "b0", Dir: bindDir, ReadOnly: false},
				{Tag: "b1", Dir: singleDir, ReadOnly: true},
			},
			Rosetta: false,
			Disks: []vz.Disk{
				{Path: rootfsDisk, ReadOnly: false},   // /dev/vda
				{Path: layerFiles[0], ReadOnly: true}, // /dev/vdb
				{Path: layerFiles[1], ReadOnly: true}, // /dev/vdc
				{Path: volDisk, ReadOnly: false},      // /dev/vdd
			},
			NAT:     true,
			Balloon: true,
		}

		vm, err := vz.New(cfg)
		if err != nil {
			t.Fatalf("vz.New: %v", err)
		}
		defer vm.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := vm.Start(ctx); err != nil {
			t.Fatalf("vm.Start: %v", err)
		}

		hello := waitHello(t, ctx, vm)
		if hello.Rosetta {
			t.Fatalf("expected rosetta=false for arm64 native VM")
		}
		if !hello.HasFeature(guestproto.FeatureContainers) {
			t.Fatalf("hello missing containers feature: %+v", hello.Features)
		}

		// Mount /dev/vda -> /var/lib/gvx.
		resp, err := vzRoundTrip(ctx, vm, guestproto.Request{
			Op:   guestproto.OpDiskMount,
			Disk: &guestproto.DiskRequest{Format: true},
		})
		if err != nil || !resp.OK {
			t.Fatalf("disk-mount: err=%v resp=%+v", err, resp)
		}

		// Start container with 2 fastpath EROFS layers + binds + volume + tmpfs + privileged.
		resp, err = vzRoundTrip(ctx, vm, guestproto.Request{
			Op: guestproto.OpContainerStart,
			Container: &guestproto.ContainerSpec{
				Argv:        []string{"/bin/ctrtest", "daemon"},
				Env:         []string{"MY_ENV=hello_microvm"},
				Workdir:     "/work",
				Hostname:    "microvm-arm64",
				ExtraHosts:  []string{"custom.local:10.1.2.3", "gw.local:host-gateway"},
				Interactive: true,
				Privileged:  true,
				MemoryMax:   256 << 20,
				PidsLimit:   64,
				Layers: []guestproto.LayerSource{
					{Device: "/dev/vdb"},
					{Device: "/dev/vdc"},
				},
				Mounts: []guestproto.ContainerMount{
					{Kind: "bind", Source: "/mnt/bins/ctrtest-arm64", Target: "/bin/ctrtest", ReadOnly: true},
					{Kind: "bind", Source: "/mnt/b0", Target: "/hostbind", ReadOnly: false},
					{Kind: "bind", Source: "/mnt/b1/single.txt", Target: "/single.txt", ReadOnly: true},
					{Kind: "volume", Source: "/dev/vdd", Target: "/vol"},
					{Kind: "tmpfs", Target: "/tmpfs-test", Options: "size=16m"},
				},
			},
		})
		if err != nil || !resp.OK || resp.Container == nil || !resp.Container.Running {
			t.Fatalf("container-start: err=%v resp=%+v", err, resp)
		}
		t.Logf("container started: pid=%d", resp.Container.PID)

		// Attach and read startup self-check output from container PID 1.
		attachConn, err := vm.Connect(ctx, guestproto.AgentPort)
		if err != nil {
			t.Fatalf("attach connect: %v", err)
		}
		defer attachConn.Close()
		if err := vmproto.WriteLine(attachConn, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerAttach}); err != nil {
			t.Fatal(err)
		}
		attachBR := bufio.NewReader(attachConn)
		var attachResp guestproto.Response
		if err := vmproto.ReadLine(attachBR, &attachResp); err != nil || !attachResp.OK {
			t.Fatalf("attach resp: err=%v resp=%+v", err, attachResp)
		}
		// Read until we see "READY\n" from daemon.
		var initOut bytes.Buffer
		for !strings.Contains(initOut.String(), "READY\n") {
			kind, payload, err := guestproto.ReadFrame(attachBR)
			if err != nil {
				t.Fatalf("attach read before READY: %v (got %q)", err, initOut.String())
			}
			if kind == guestproto.FrameStdout || kind == guestproto.FrameStderr {
				initOut.Write(payload)
			} else if kind == guestproto.FrameExit {
				t.Fatalf("container exited early: %s (out=%q)", payload, initOut.String())
			}
		}
		t.Logf("container PID 1 check output:\n%s", initOut.String())
		for _, want := range []string{
			"ARCH=arm64",
			"HOSTNAME=microvm-arm64",
			"PWD=/work",
			"ENV=hello_microvm",
			"LAYER_MOTD=ok",
			"LAYER_NEW=new",
			"LAYER_OPAQUE_Y=ok",
			"LAYER_WHITEOUT_A=absent",
			"LAYER_OPAQUE_X=absent",
			"HOSTBIND=host-bind-ok",
			"SINGLE=single-file-ok",
			"TUN=ok",
			"FUSE=ok",
			"OLDROOT_HIDDEN=ok",
			"DOCKER_INTERNAL=ok",
			"EXTRA_HOST=ok",
		} {
			if !strings.Contains(initOut.String(), want) {
				t.Errorf("PID 1 output missing %q", want)
			}
		}

		// ContainerExec via busybox nsenter!
		execOut, execErr, execExit := vzCtrExec(t, ctx, vm, guestproto.ContainerExecRequest{
			Argv:        []string{"/bin/ctrtest", "exec-check"},
			Env:         []string{"EXEC_VAR=nsenter_ok"},
			Workdir:     "/vol",
			Interactive: true,
		}, "stdin-to-exec\n")
		t.Logf("exec out=%q err=%q exit=%+v", execOut, execErr, execExit)
		if execExit.Code != 0 {
			t.Fatalf("exec failed: exit=%+v err=%q", execExit, execErr)
		}
		for _, want := range []string{"EXEC_HOSTNAME=microvm-arm64", "EXEC_PWD=/vol", "EXEC_VAR=nsenter_ok", "EXEC_STDIN=stdin-to-exec", "EXEC_PROC_PID1=ok"} {
			if !strings.Contains(execOut, want) {
				t.Errorf("exec output missing %q (got %q)", want, execOut)
			}
		}

		// Top & Stats.
		resp, err = vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpContainerTop})
		if err != nil || !resp.OK || len(resp.Processes) == 0 {
			t.Fatalf("top: err=%v resp=%+v", err, resp)
		}
		t.Logf("top: %+v", resp.Processes)

		resp, err = vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpContainerStats})
		if err != nil || !resp.OK || resp.Stats == nil || resp.Stats.PIDs < 1 || resp.Stats.MemoryLimitBytes != 256<<20 {
			t.Fatalf("stats: err=%v resp=%+v", err, resp)
		}
		t.Logf("stats: mem=%d/%d pids=%d/%d net=%+v", resp.Stats.MemoryBytes, resp.Stats.MemoryLimitBytes, resp.Stats.PIDs, resp.Stats.PIDsLimit, resp.Stats.Network)

		// Pause & Unpause.
		if resp, err := vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpContainerPause}); err != nil || !resp.OK {
			t.Fatalf("pause: err=%v resp=%+v", err, resp)
		}
		if resp, err := vzRoundTrip(ctx, vm, guestproto.Request{
			Op:            guestproto.OpContainerExec,
			ContainerExec: &guestproto.ContainerExecRequest{Argv: []string{"/bin/ctrtest", "exec-check"}},
		}); err != nil || resp.OK || resp.Code != guestproto.CodePaused {
			t.Fatalf("exec while paused: err=%v resp=%+v", err, resp)
		}
		if resp, err := vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpContainerUnpause}); err != nil || !resp.OK {
			t.Fatalf("unpause: err=%v resp=%+v", err, resp)
		}

		// CopyTo -> Stat -> CopyFrom -> Export(UpperOnly) (including whiteout!).
		var uploadBuf bytes.Buffer
		tw := tar.NewWriter(&uploadBuf)
		_ = tw.WriteHeader(&tar.Header{Name: "copied.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 7})
		_, _ = tw.Write([]byte("payload"))
		_ = tw.Close()
		vzCtrCopyTo(t, ctx, vm, "/work", uploadBuf.Bytes())

		resp, err = vzRoundTrip(ctx, vm, guestproto.Request{
			Op:            guestproto.OpContainerStat,
			ContainerCopy: &guestproto.ContainerCopyRequest{Path: "/work/copied.txt"},
		})
		if err != nil || !resp.OK || resp.PathStat == nil || resp.PathStat.Size != 7 {
			t.Fatalf("stat copied.txt: err=%v resp=%+v", err, resp)
		}

		copyFromTar := vzCtrCopyFrom(t, ctx, vm, "/work/copied.txt")
		if names := listTarNames(t, copyFromTar); !slices.Equal(names, []string{"copied.txt"}) {
			t.Fatalf("copy-from tar = %v", names)
		}

		// Delete a file from lower layer via exec so overlayfs creates a real whiteout in upperdir!
		_, _, delExit := vzCtrExec(t, ctx, vm, guestproto.ContainerExecRequest{
			Argv: []string{"/bin/ctrtest", "rm-motd"},
		}, "")
		if delExit.Code != 0 {
			t.Fatalf("rm-motd exit=%+v", delExit)
		}

		upperTar := vzCtrExport(t, ctx, vm, guestproto.ContainerExportRequest{UpperOnly: true})
		upperNames := listTarNames(t, upperTar)
		t.Logf("upperOnly export entries: %v", upperNames)
		if !slices.Contains(upperNames, "work/copied.txt") || !slices.Contains(upperNames, "etc/.wh.motd") {
			t.Fatalf("upperOnly export missing expected entries: %v", upperNames)
		}

		// Graceful shutdown stops container, unmounts everything, powers off cleanly!
		if resp, err := vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpShutdown}); err != nil || !resp.OK {
			t.Fatalf("shutdown: err=%v resp=%+v", err, resp)
		}
		select {
		case <-vm.Done():
			t.Logf("arm64 container VM graceful shutdown OK (err=%v)", vm.Err())
		case <-time.After(10 * time.Second):
			_ = vm.Stop(ctx)
			t.Fatalf("arm64 VM did not power off")
		}
	})

	// Subtest 2: amd64 (Rosetta ON) + overflow loop.ko EROFS path + ReadonlyRootfs + PTY!
	t.Run("amd64_rosetta_loop_erofs_pty", func(t *testing.T) {
		runDir := filepath.Join(workDir, "amd64")
		layersDir := filepath.Join(runDir, "layers")
		_ = os.MkdirAll(layersDir, 0o755)
		for i, src := range layerFiles[:2] {
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(layersDir, fmt.Sprintf("l%d.erofs", i)), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}

		consolePath := filepath.Join(runDir, "console.log")
		consoleFile, err := os.Create(consolePath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = consoleFile.Close()
			if b, err := os.ReadFile(consolePath); err == nil && len(b) > 0 {
				t.Logf("amd64 console:\n%s", string(b))
			}
		}()

		cfg := vz.Config{
			Kernel:      filepath.Join(assetsDir, "Image"),
			Initrd:      filepath.Join(assetsDir, "initramfs.gz"),
			CommandLine: "console=hvc0 panic=-1 quiet gvx.rosetta=rosetta gvx.shares=layers:ro,bins:ro",
			CPUs:        2,
			MemoryBytes: 512 << 20,
			Console:     consoleFile,
			Shares: []vz.Share{
				{Tag: "layers", Dir: layersDir, ReadOnly: true},
				{Tag: "bins", Dir: binDir, ReadOnly: true},
			},
			Rosetta:    true,
			RosettaTag: guestproto.RosettaTag,
			NAT:        false,
			Balloon:    true,
		}

		vm, err := vz.New(cfg)
		if err != nil {
			t.Fatalf("vz.New: %v", err)
		}
		defer vm.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := vm.Start(ctx); err != nil {
			t.Fatalf("vm.Start: %v", err)
		}

		hello := waitHello(t, ctx, vm)
		if !hello.Rosetta {
			t.Fatalf("expected rosetta=true")
		}

		resp, err := vzRoundTrip(ctx, vm, guestproto.Request{
			Op: guestproto.OpContainerStart,
			Container: &guestproto.ContainerSpec{
				Argv:           []string{"/bin/ctrtest", "oneshot"},
				Hostname:       "microvm-amd64",
				TTY:            true,
				Rows:           24,
				Cols:           80,
				ReadonlyRootfs: false, // tmpfs upperdir since no /dev/vda
				Layers: []guestproto.LayerSource{
					{File: "/mnt/layers/l0.erofs"},
					{File: "/mnt/layers/l1.erofs"},
				},
				Mounts: []guestproto.ContainerMount{
					{Kind: "bind", Source: "/mnt/bins/ctrtest-amd64", Target: "/bin/ctrtest", ReadOnly: true},
				},
			},
		})
		if err != nil || !resp.OK || resp.Container == nil {
			t.Fatalf("amd64 container-start: err=%v resp=%+v", err, resp)
		}

		// Attach (even if oneshot already exited!) and verify buffered output + exit code.
		attachConn, err := vm.Connect(ctx, guestproto.AgentPort)
		if err != nil {
			t.Fatal(err)
		}
		defer attachConn.Close()
		_ = vmproto.WriteLine(attachConn, guestproto.Request{V: guestproto.Version, Op: guestproto.OpContainerAttach})
		br := bufio.NewReader(attachConn)
		var ar guestproto.Response
		if err := vmproto.ReadLine(br, &ar); err != nil || !ar.OK {
			t.Fatalf("amd64 attach: err=%v resp=%+v", err, ar)
		}
		var out bytes.Buffer
		var ex guestproto.Exit
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("amd64 attach frame: %v", err)
			}
			if kind == guestproto.FrameStdout {
				out.Write(payload)
			} else if kind == guestproto.FrameExit {
				_ = json.Unmarshal(payload, &ex)
				break
			}
		}
		t.Logf("amd64 oneshot out=%q exit=%+v", out.String(), ex)
		if ex.Code != 0 || !strings.Contains(out.String(), "ARCH=amd64") || !strings.Contains(out.String(), "LAYER_NEW=new") || !strings.Contains(out.String(), "OLDROOT_HIDDEN=ok") {
			t.Fatalf("unexpected amd64 oneshot result: out=%q exit=%+v", out.String(), ex)
		}

		if resp, err := vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpShutdown}); err != nil || !resp.OK {
			t.Fatalf("amd64 shutdown: err=%v resp=%+v", err, resp)
		}
		select {
		case <-vm.Done():
		case <-time.After(10 * time.Second):
			_ = vm.Stop(ctx)
			t.Fatalf("amd64 VM did not power off")
		}
	})
}

func makeSparseDisk(t *testing.T, path string, size int64) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitHello(t *testing.T, ctx context.Context, vm *vz.VM) guestproto.Hello {
	t.Helper()
	t0 := time.Now()
	for {
		resp, err := vzRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpHello})
		if err == nil && resp.OK && resp.Hello != nil {
			return *resp.Hello
		}
		if time.Since(t0) > 10*time.Second {
			t.Fatalf("agent did not answer hello: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func vzCtrExec(t *testing.T, ctx context.Context, vm *vz.VM, req guestproto.ContainerExecRequest, stdin string) (string, string, guestproto.Exit) {
	t.Helper()
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		t.Fatalf("ctr exec connect: %v", err)
	}
	defer conn.Close()
	if err := vmproto.WriteLine(conn, guestproto.Request{
		V:             guestproto.Version,
		Op:            guestproto.OpContainerExec,
		ContainerExec: &req,
	}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil || !resp.OK {
		t.Fatalf("ctr exec resp: err=%v resp=%+v", err, resp)
	}
	if req.Interactive {
		go func() {
			if stdin != "" {
				_ = guestproto.WriteFrame(conn, guestproto.FrameStdin, []byte(stdin))
			}
			_ = guestproto.WriteFrame(conn, guestproto.FrameStdinEOF, nil)
		}()
	}
	var out, errOut bytes.Buffer
	var ex guestproto.Exit
	for {
		kind, payload, err := guestproto.ReadFrame(br)
		if err != nil {
			t.Fatalf("ctr exec frame: %v", err)
		}
		switch kind {
		case guestproto.FrameStdout:
			out.Write(payload)
		case guestproto.FrameStderr:
			errOut.Write(payload)
		case guestproto.FrameExit:
			_ = json.Unmarshal(payload, &ex)
			return out.String(), errOut.String(), ex
		}
	}
}

func vzCtrCopyTo(t *testing.T, ctx context.Context, vm *vz.VM, dstPath string, tarBytes []byte) {
	t.Helper()
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = vmproto.WriteLine(conn, guestproto.Request{
		V:             guestproto.Version,
		Op:            guestproto.OpContainerCopyTo,
		ContainerCopy: &guestproto.ContainerCopyRequest{Path: dstPath},
	})
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil || !resp.OK {
		t.Fatalf("copy-to resp: err=%v resp=%+v", err, resp)
	}
	_ = guestproto.WriteFrame(conn, guestproto.FrameData, tarBytes)
	_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
	kind, payload, err := guestproto.ReadFrame(br)
	if err != nil || kind != guestproto.FrameEOF || len(payload) != 0 {
		t.Fatalf("copy-to eof: kind=%d payload=%q err=%v", kind, payload, err)
	}
}

func vzCtrCopyFrom(t *testing.T, ctx context.Context, vm *vz.VM, srcPath string) []byte {
	t.Helper()
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = vmproto.WriteLine(conn, guestproto.Request{
		V:             guestproto.Version,
		Op:            guestproto.OpContainerCopyFrom,
		ContainerCopy: &guestproto.ContainerCopyRequest{Path: srcPath},
	})
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil || !resp.OK {
		t.Fatalf("copy-from resp: err=%v resp=%+v", err, resp)
	}
	return readTarFrames(t, br)
}

func vzCtrExport(t *testing.T, ctx context.Context, vm *vz.VM, req guestproto.ContainerExportRequest) []byte {
	t.Helper()
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = vmproto.WriteLine(conn, guestproto.Request{
		V:               guestproto.Version,
		Op:              guestproto.OpContainerExport,
		ContainerExport: &req,
	})
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil || !resp.OK {
		t.Fatalf("export resp: err=%v resp=%+v", err, resp)
	}
	return readTarFrames(t, br)
}

func readTarFrames(t *testing.T, br *bufio.Reader) []byte {
	t.Helper()
	var buf bytes.Buffer
	for {
		kind, payload, err := guestproto.ReadFrame(br)
		if err != nil {
			t.Fatalf("read tar frame: %v", err)
		}
		if kind == guestproto.FrameData {
			buf.Write(payload)
		} else if kind == guestproto.FrameEOF {
			if len(payload) > 0 {
				t.Fatalf("tar stream error: %s", payload)
			}
			return buf.Bytes()
		}
	}
}

func buildCtrTestBinary(t *testing.T, outBin, goarch string) {
	t.Helper()
	srcDir := t.TempDir()
	const src = `package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

func main() {
	mode := "oneshot"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "oneshot", "daemon":
		fmt.Printf("ARCH=%s\n", runtime.GOARCH)
		hn, _ := os.Hostname()
		fmt.Printf("HOSTNAME=%s\n", hn)
		wd, _ := os.Getwd()
		fmt.Printf("PWD=%s\n", wd)
		fmt.Printf("ENV=%s\n", os.Getenv("MY_ENV"))
		if _, err := os.Stat("/etc/motd"); err == nil {
			fmt.Println("LAYER_MOTD=ok")
		}
		if b, err := os.ReadFile("/opt/new"); err == nil {
			fmt.Printf("LAYER_NEW=%s\n", strings.TrimSpace(string(b)))
		}
		if _, err := os.Stat("/etc/sub/y"); err == nil {
			fmt.Println("LAYER_OPAQUE_Y=ok")
		}
		if _, err := os.Stat("/etc/a.txt"); os.IsNotExist(err) {
			fmt.Println("LAYER_WHITEOUT_A=absent")
		}
		if _, err := os.Stat("/etc/sub/x"); os.IsNotExist(err) {
			fmt.Println("LAYER_OPAQUE_X=absent")
		}
		if b, err := os.ReadFile("/hostbind/from-host.txt"); err == nil {
			fmt.Printf("HOSTBIND=%s\n", strings.TrimSpace(string(b)))
		}
		if b, err := os.ReadFile("/single.txt"); err == nil {
			fmt.Printf("SINGLE=%s\n", strings.TrimSpace(string(b)))
		}
		if _, err := os.Stat("/dev/net/tun"); err == nil {
			fmt.Println("TUN=ok")
		}
		if _, err := os.Stat("/dev/fuse"); err == nil {
			fmt.Println("FUSE=ok")
		}
		if _, err := os.Stat("/mnt/layers"); os.IsNotExist(err) {
			if _, err2 := os.Stat("/var/lib/gvx"); os.IsNotExist(err2) {
				fmt.Println("OLDROOT_HIDDEN=ok")
			}
		}
		if b, err := os.ReadFile("/etc/hosts"); err == nil {
			s := string(b)
			if strings.Contains(s, "host.docker.internal") {
				fmt.Println("DOCKER_INTERNAL=ok")
			}
			if strings.Contains(s, "10.1.2.3\tcustom.local") {
				fmt.Println("EXTRA_HOST=ok")
			}
		}
		fmt.Println("READY")
		if mode == "daemon" {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
			<-ch
		}
	case "exec-check":
		hn, _ := os.Hostname()
		wd, _ := os.Getwd()
		in, _ := io.ReadAll(os.Stdin)
		fmt.Printf("EXEC_HOSTNAME=%s\n", hn)
		fmt.Printf("EXEC_PWD=%s\n", wd)
		fmt.Printf("EXEC_VAR=%s\n", os.Getenv("EXEC_VAR"))
		fmt.Printf("EXEC_STDIN=%s\n", strings.TrimSpace(string(in)))
		if _, err := os.Stat("/proc/1/cmdline"); err == nil {
			fmt.Println("EXEC_PROC_PID1=ok")
		}
	case "rm-motd":
		if err := os.Remove("/etc/motd"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module ctrtest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", outBin, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ctrtest %s: %v\n%s", goarch, err, string(out))
	}
}
