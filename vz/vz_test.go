package vz

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

func TestStrings(t *testing.T) {
	if got := RosettaInstalled.String(); got != "installed" {
		t.Fatalf("RosettaInstalled.String() = %q", got)
	}
	if got := RosettaNotInstalled.String(); got != "not-installed" {
		t.Fatalf("RosettaNotInstalled.String() = %q", got)
	}
	if got := RosettaNotSupported.String(); got != "not-supported" {
		t.Fatalf("RosettaNotSupported.String() = %q", got)
	}
	if got := StateRunning.String(); got != "running" {
		t.Fatalf("StateRunning.String() = %q", got)
	}
	if got := StateStopped.String(); got != "stopped" {
		t.Fatalf("StateStopped.String() = %q", got)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New(Config{}) succeeded, want error")
	}
	if _, err := New(Config{Kernel: "/nonexistent/Image", CPUs: 0, MemoryBytes: 1 << 30}); err == nil {
		t.Fatal("New with 0 CPUs succeeded, want error")
	}
	if _, err := New(Config{Kernel: "/nonexistent/Image", CPUs: 1, MemoryBytes: 0}); err == nil {
		t.Fatal("New with 0 memory succeeded, want error")
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// Huge CPU count must fail VZ validation (NSError out-param).
		if _, err := New(Config{Kernel: "/nonexistent/Image", CPUs: 99999, MemoryBytes: 512 << 20}); err == nil {
			t.Fatal("New with 99999 CPUs succeeded, want VZ validation error")
		}
	}
}

func TestRosettaAvailabilityDoesNotCrash(t *testing.T) {
	avail := RosettaAvailability()
	switch avail {
	case RosettaNotSupported, RosettaNotInstalled, RosettaInstalled:
	default:
		t.Fatalf("unexpected Availability %d", avail)
	}
}

// TestVZRealPrototype boots the prototype fixture in /tmp/gvxd-vzs directly
// through internal/vz. Requires a signed test binary (com.apple.security.virtualization)
// and GVX_VZ_TEST=1.
func TestVZRealPrototype(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_VZ_TEST") == "" {
		t.Skip("set GVX_VZ_TEST=1 (and sign test binary with entitlements) to run real VZ test")
	}
	if !Supported() {
		t.Fatal("vz.Supported() = false (missing entitlement?)")
	}
	if avail := RosettaAvailability(); avail != RosettaInstalled {
		t.Fatalf("RosettaAvailability() = %v, want installed", avail)
	}

	fixture := os.Getenv("GVX_VZ_FIXTURE")
	if fixture == "" {
		fixture = "/tmp/gvxd-vzs"
	}
	kernel := filepath.Join(fixture, "build", "Image")
	initrd := filepath.Join(fixture, "build", "initramfs.gz")
	amd64Root := filepath.Join(fixture, "amd64root")
	for _, p := range []string{kernel, initrd, amd64Root} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("fixture missing %s: %v", p, err)
		}
	}

	tmpDir, err := os.MkdirTemp("/tmp", "gvxd-vzt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "disk.img")
	f, err := os.Create(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	consolePath := filepath.Join(tmpDir, "console.log")
	consoleFile, err := os.Create(consolePath)
	if err != nil {
		t.Fatal(err)
	}
	defer consoleFile.Close()

	cfg := Config{
		Kernel:      kernel,
		Initrd:      initrd,
		CommandLine: "console=hvc0 panic=-1 quiet vz.rosetta=rosetta vz.shares=amd64:ro vz.net=1",
		CPUs:        2,
		MemoryBytes: 1024 << 20,
		Console:     consoleFile,
		Rosetta:     true,
		RosettaTag:  "rosetta",
		Shares:      []Share{{Tag: "amd64", Dir: amd64Root, ReadOnly: true}},
		Disks:       []Disk{{Path: diskPath}},
		NAT:         true,
		Balloon:     true,
	}

	t0 := time.Now()
	vm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = vm.Stop(stopCtx)
		_ = vm.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := vm.State(); st != StateRunning {
		t.Fatalf("State after Start = %v, want running", st)
	}

	// Poll prototype agent on vsock port 1024.
	var conn io.ReadWriteCloser
	for {
		c, err := vm.Connect(ctx, 1024)
		if err == nil {
			conn = c
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("connect timeout: last err %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("agent accepted vsock in %s", time.Since(t0).Round(time.Millisecond))
	fmt.Fprintf(conn, `{"op":"ping"}`+"\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if err != nil || !strings.Contains(line, "pong") {
		t.Fatalf("ping = %q, err = %v", line, err)
	}

	// Closed port must return NSError.
	if _, err := vm.Connect(ctx, 4242); err == nil {
		t.Fatal("Connect(4242) succeeded, want error")
	}

	// Run chroot x86_64 uname -m via Rosetta.
	runProto := func(argv ...string) string {
		c, err := vm.Connect(ctx, 1024)
		if err != nil {
			t.Fatalf("connect for %v: %v", argv, err)
		}
		defer c.Close()
		b, _ := json.Marshal(map[string]any{"op": "exec", "argv": argv})
		if _, err := c.Write(append(b, '\n')); err != nil {
			t.Fatalf("write exec: %v", err)
		}
		r := bufio.NewReader(c)
		var out strings.Builder
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				t.Fatalf("read frame: %v", err)
			}
			p := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
			if _, err := io.ReadFull(r, p); err != nil {
				t.Fatalf("read payload: %v", err)
			}
			switch hdr[0] {
			case 1, 2:
				out.Write(p)
			case 3:
				return out.String()
			}
		}
	}

	arch := strings.TrimSpace(runProto("sh", "-c", "mount -t proc proc /mnt/amd64/proc && chroot /mnt/amd64 /bin/sh -c 'uname -m'"))
	if arch != "x86_64" {
		t.Fatalf("chroot uname -m = %q, want x86_64", arch)
	}
	t.Logf("rosetta chroot uname -m = %s", arch)

	// Guest-initiated poweroff should fire delegate and close Done.
	go func() {
		c, err := vm.Connect(context.Background(), 1024)
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := json.Marshal(map[string]any{"op": "exec", "argv": []string{"poweroff", "-f"}})
		_, _ = c.Write(append(b, '\n'))
	}()

	select {
	case <-vm.Done():
		if err := vm.Err(); err != nil {
			t.Fatalf("vm.Err() after poweroff = %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = vm.Stop(context.Background())
		t.Fatal("timed out waiting for vm.Done() after poweroff")
	}

	// Verify console log got output.
	if info, err := os.Stat(consolePath); err != nil || info.Size() == 0 {
		t.Fatalf("console.log missing or empty: err=%v", err)
	}
	if err := vm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second boot to test forced Stop.
	vm2, err := New(cfg)
	if err != nil {
		t.Fatalf("New(2): %v", err)
	}
	defer vm2.Close()
	if err := vm2.Start(ctx); err != nil {
		t.Fatalf("Start(2): %v", err)
	}
	if err := vm2.Stop(ctx); err != nil {
		t.Fatalf("Stop(2): %v", err)
	}
	select {
	case <-vm2.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("vm2.Done() not closed after Stop")
	}
	if st := vm2.State(); st != StateStopped {
		t.Fatalf("vm2.State() = %v, want stopped", st)
	}
}

// TestVZRealGuestAssets boots GVX_VM_ASSETS (workstream B's guestproto assets)
// directly via internal/vz, verifies Rosetta chroot uname -m = x86_64 via
// guestproto OpExec, and proves the persistent ext4 disk formats once and
// preserves data across a graceful shutdown and restart.
func TestVZRealGuestAssets(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_VZ_TEST") == "" {
		t.Skip("set GVX_VZ_TEST=1 and GVX_VM_ASSETS=<dir> (with signed test binary) to run")
	}
	assetsDir := os.Getenv("GVX_VM_ASSETS")
	if assetsDir == "" {
		t.Skip("GVX_VM_ASSETS not set")
	}
	amd64Root := os.Getenv("GVX_AMD64_ROOT")
	if amd64Root == "" {
		amd64Root = "/tmp/gvxd-vzs/amd64root"
	}
	kernel := filepath.Join(assetsDir, "Image")
	initrd := filepath.Join(assetsDir, "initramfs.gz")
	for _, p := range []string{kernel, initrd, amd64Root} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing %s: %v", p, err)
		}
	}

	tmpDir, err := os.MkdirTemp("/tmp", "gvxd-vza-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "disk.img")
	df, err := os.Create(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := df.Truncate(256 << 20); err != nil {
		df.Close()
		t.Fatal(err)
	}
	df.Close()

	cfg := Config{
		Kernel:      kernel,
		Initrd:      initrd,
		CommandLine: "console=hvc0 panic=-1 quiet gvx.assets=assets gvx.rosetta=rosetta gvx.net=1 gvx.shares=amd64:ro",
		CPUs:        2,
		MemoryBytes: 1024 << 20,
		Rosetta:     true,
		RosettaTag:  guestproto.RosettaTag,
		Shares: []Share{
			{Tag: guestproto.AssetsTag, Dir: assetsDir, ReadOnly: true},
			{Tag: "amd64", Dir: amd64Root, ReadOnly: true},
		},
		Disks:   []Disk{{Path: diskPath}},
		NAT:     true,
		Balloon: true,
	}

	roundTrip := func(ctx context.Context, vm *VM, req guestproto.Request) guestproto.Response {
		c, err := vm.Connect(ctx, guestproto.AgentPort)
		if err != nil {
			t.Fatalf("connect for %s: %v", req.Op, err)
		}
		defer c.Close()
		req.V = guestproto.Version
		if err := vmproto.WriteLine(c, req); err != nil {
			t.Fatalf("write %s: %v", req.Op, err)
		}
		var resp guestproto.Response
		if err := vmproto.ReadLine(bufio.NewReader(c), &resp); err != nil {
			t.Fatalf("read %s: %v", req.Op, err)
		}
		if !resp.OK {
			t.Fatalf("op %s failed: %+v", req.Op, resp)
		}
		return resp
	}

	execCmd := func(ctx context.Context, vm *VM, argv ...string) string {
		c, err := vm.Connect(ctx, guestproto.AgentPort)
		if err != nil {
			t.Fatalf("connect exec: %v", err)
		}
		defer c.Close()
		if err := vmproto.WriteLine(c, guestproto.Request{
			V:    guestproto.Version,
			Op:   guestproto.OpExec,
			Exec: &guestproto.ExecRequest{Argv: argv},
		}); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(c)
		var resp guestproto.Response
		if err := vmproto.ReadLine(br, &resp); err != nil || !resp.OK {
			t.Fatalf("exec resp: %+v err=%v", resp, err)
		}
		var out strings.Builder
		for {
			kind, p, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			switch kind {
			case guestproto.FrameStdout, guestproto.FrameStderr:
				out.Write(p)
			case guestproto.FrameExit:
				var ex guestproto.Exit
				if err := json.Unmarshal(p, &ex); err != nil || ex.Code != 0 {
					t.Fatalf("exec %v exited %+v (output: %q)", argv, ex, out.String())
				}
				return out.String()
			}
		}
	}

	bootAndWait := func(ctx context.Context) *VM {
		vm, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := vm.Start(ctx); err != nil {
			vm.Close()
			t.Fatalf("Start: %v", err)
		}
		for {
			c, err := vm.Connect(ctx, guestproto.AgentPort)
			if err == nil {
				_ = vmproto.WriteLine(c, guestproto.Request{V: guestproto.Version, Op: guestproto.OpHello})
				var r guestproto.Response
				err = vmproto.ReadLine(bufio.NewReader(c), &r)
				c.Close()
				if err == nil && r.OK {
					return vm
				}
			}
			if ctx.Err() != nil {
				_ = vm.Stop(context.Background())
				vm.Close()
				t.Fatalf("agent ready timeout")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Boot 1: format disk, write marker, verify Rosetta x86_64, graceful shutdown.
	vm1 := bootAndWait(ctx)
	d1 := roundTrip(ctx, vm1, guestproto.Request{Op: guestproto.OpDiskMount, Disk: &guestproto.DiskRequest{Format: true}})
	if d1.Disk == nil || !d1.Disk.Formatted || d1.Disk.FSType != "ext4" {
		_ = vm1.Stop(context.Background())
		vm1.Close()
		t.Fatalf("boot 1 disk-mount = %+v, want formatted ext4", d1.Disk)
	}
	const sentinel = "persist-ok-42"
	execCmd(ctx, vm1, "sh", "-c", "printf '%s' '"+sentinel+"' > /var/lib/gvx/persist.txt")

	arch := strings.TrimSpace(execCmd(ctx, vm1, "sh", "-c", "mount -t proc proc /mnt/amd64/proc && chroot /mnt/amd64 /bin/sh -c 'uname -m'"))
	if arch != "x86_64" {
		_ = vm1.Stop(context.Background())
		vm1.Close()
		t.Fatalf("Rosetta chroot uname -m = %q, want x86_64", arch)
	}
	t.Logf("guestproto exec Rosetta chroot uname -m = %s", arch)

	roundTrip(ctx, vm1, guestproto.Request{Op: guestproto.OpShutdown})
	select {
	case <-vm1.Done():
	case <-time.After(10 * time.Second):
		_ = vm1.Stop(context.Background())
		t.Fatal("boot 1 shutdown timeout")
	}
	vm1.Close()

	// Boot 2: mount existing disk (must NOT reformat) and verify sentinel survived.
	vm2 := bootAndWait(ctx)
	defer func() {
		_ = vm2.Stop(context.Background())
		vm2.Close()
	}()
	d2 := roundTrip(ctx, vm2, guestproto.Request{Op: guestproto.OpDiskMount, Disk: &guestproto.DiskRequest{Format: true}})
	if d2.Disk == nil || d2.Disk.Formatted {
		t.Fatalf("boot 2 disk-mount = %+v, want Formatted=false", d2.Disk)
	}
	got := execCmd(ctx, vm2, "cat", "/var/lib/gvx/persist.txt")
	if got != sentinel {
		t.Fatalf("persisted file = %q, want %q", got, sentinel)
	}
	t.Logf("persistent disk survived restart (formatted=%v, content=%q)", d2.Disk.Formatted, got)

	roundTrip(ctx, vm2, guestproto.Request{Op: guestproto.OpShutdown})
	select {
	case <-vm2.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("boot 2 shutdown timeout")
	}
}

// TestVZCancelDuringStart verifies that canceling Start(ctx) while
// startWithCompletionHandler: is still in flight followed by Stop+Close (or
// direct Close) neither crashes nor leaves an orphaned
// com.apple.Virtualization.VirtualMachine XPC process behind.
func TestVZCancelDuringStart(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_VZ_TEST") == "" {
		t.Skip("set GVX_VZ_TEST=1 (with signed test binary) to run real VZ cancel-during-start test")
	}
	if !Supported() {
		t.Fatal("vz.Supported() = false (missing entitlement?)")
	}
	var kernel, initrd string
	if assetsDir := os.Getenv("GVX_VM_ASSETS"); assetsDir != "" {
		kernel = filepath.Join(assetsDir, "Image")
		initrd = filepath.Join(assetsDir, "initramfs.gz")
	} else {
		fixture := os.Getenv("GVX_VZ_FIXTURE")
		if fixture == "" {
			fixture = "/tmp/gvxd-vzs"
		}
		kernel = filepath.Join(fixture, "build", "Image")
		initrd = filepath.Join(fixture, "build", "initramfs.gz")
	}
	for _, p := range []string{kernel, initrd} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("fixture missing %s: %v", p, err)
		}
	}

	countVZXPC := func() int {
		out, err := exec.Command("pgrep", "-f", "com.apple.Virtualization.VirtualMachine").Output()
		if err != nil {
			return 0
		}
		n := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(line) != "" {
				n++
			}
		}
		return n
	}

	cfg := Config{
		Kernel:      kernel,
		Initrd:      initrd,
		CommandLine: "console=hvc0 panic=-1 quiet",
		CPUs:        2,
		MemoryBytes: 512 << 20,
		Balloon:     true,
	}

	baseXPC := countVZXPC()

	for _, tc := range []struct {
		name     string
		callStop bool
	}{
		{name: "cancel-then-stop-and-close", callStop: true},
		{name: "cancel-then-close-only", callStop: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			// Cancel immediately (1ms) so startWithCompletionHandler: is still in flight.
			startCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			startErr := vm.Start(startCtx)
			t.Logf("Start returned: %v", startErr)

			if tc.callStop {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer stopCancel()
				if err := vm.Stop(stopCtx); err != nil {
					t.Fatalf("Stop after canceled Start: %v", err)
				}
			}
			if err := vm.Close(); err != nil {
				t.Fatalf("Close after canceled Start: %v", err)
			}
			if st := vm.State(); st != StateStopped {
				t.Fatalf("State after Close = %v, want stopped", st)
			}

			deadline := time.Now().Add(5 * time.Second)
			for {
				if cur := countVZXPC(); cur <= baseXPC {
					t.Logf("VZ XPC count returned to %d (baseline %d)", cur, baseXPC)
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("orphaned VZ XPC process left after %s: got %d, baseline %d", tc.name, countVZXPC(), baseXPC)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}
