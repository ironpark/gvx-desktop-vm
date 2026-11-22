package vmhelper_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

// TestHelperRealVZ exercises the real standalone CGO_ENABLED=0 helper binary
// with Virtualization.framework, vmclient, guestproto agent, persistent disk,
// Rosetta, graceful/forced stop, stdin lease close, and SIGKILL VM teardown.
//
// Run with: GVX_VZ_TEST=1 GVX_VM_ASSETS=/path/to/assets go test -v ./internal/vmhelper -run TestHelperRealVZ
func TestHelperRealVZ(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_VZ_TEST") == "" {
		t.Skip("set GVX_VZ_TEST=1 and GVX_VM_ASSETS=<dir> to run real helper integration test")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("Apple Silicon macOS only")
	}
	assetsDir := os.Getenv("GVX_VM_ASSETS")
	if assetsDir == "" {
		t.Skip("set GVX_VM_ASSETS to an asset directory containing Image and initramfs.gz")
	}
	kernel := filepath.Join(assetsDir, "Image")
	initrd := filepath.Join(assetsDir, "initramfs.gz")
	for _, p := range []string{kernel, initrd} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("asset missing %s: %v", p, err)
		}
	}

	repoRoot := findRepoRoot(t)
	tmpDir, err := os.MkdirTemp("/tmp", "gvxd-a-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// 1. Build CGO_ENABLED=0 helper (native and explicit darwin/arm64) and verify byte identity.
	binA := filepath.Join(tmpDir, "gvx-vmhelper-native")
	binB := filepath.Join(tmpDir, "gvx-vmhelper-cross")
	buildHelper := func(out string, extraEnv ...string) {
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/gvx-vmhelper")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), append([]string{"CGO_ENABLED=0"}, extraEnv...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", out, err, b)
		}
	}
	buildHelper(binA)
	buildHelper(binB, "GOOS=darwin", "GOARCH=arm64")

	bytesA, err := os.ReadFile(binA)
	if err != nil {
		t.Fatal(err)
	}
	bytesB, err := os.ReadFile(binB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytesA, bytesB) {
		t.Fatal("native and GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 builds are not byte-identical")
	}

	verOut, err := exec.Command("go", "version", "-m", binA).CombinedOutput()
	if err != nil || !strings.Contains(string(verOut), "CGO_ENABLED=0") {
		t.Fatalf("go version -m did not report CGO_ENABLED=0: err=%v\n%s", err, verOut)
	}

	// Unsigned check must report Virtualization=false.
	unsignedOut, err := exec.Command(binA, vmhelper.CmdCheck).CombinedOutput()
	if err != nil {
		t.Fatalf("unsigned check: %v\n%s", err, unsignedOut)
	}
	var unsignedRes vmhelper.CheckResult
	if err := json.Unmarshal(unsignedOut, &unsignedRes); err != nil {
		t.Fatalf("unmarshal unsigned check: %v (%s)", err, unsignedOut)
	}
	if unsignedRes.Virtualization {
		t.Fatalf("unsigned helper unexpectedly reported Virtualization=true: %+v", unsignedRes)
	}

	// Ad-hoc sign with com.apple.security.virtualization.
	entitlements := filepath.Join(repoRoot, "entitlements.plist")
	if out, err := exec.Command("codesign", "--sign", "-", "--entitlements", entitlements, "--force", binA).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v\n%s", err, out)
	}

	// Signed check must report OK() == true and Rosetta == installed.
	signedOut, err := exec.Command(binA, vmhelper.CmdCheck).CombinedOutput()
	if err != nil {
		t.Fatalf("signed check: %v\n%s", err, signedOut)
	}
	var checkRes vmhelper.CheckResult
	if err := json.Unmarshal(signedOut, &checkRes); err != nil {
		t.Fatalf("unmarshal signed check: %v (%s)", err, signedOut)
	}
	t.Logf("check result: %+v", checkRes)
	if !checkRes.OK() || checkRes.Rosetta != vmhelper.RosettaInstalled || checkRes.MacOS == "" {
		t.Fatalf("signed check not OK: %+v", checkRes)
	}

	// 2. Launch helper via vmclient over 0600 unix socket.
	stateDir := filepath.Join(tmpDir, "state")
	launcher := vmclient.Launcher{
		Path:         binA,
		Args:         []string{vmhelper.CmdServe},
		Dir:          stateDir,
		Log:          func(line string) { t.Logf("helper stderr: %s", line) },
		StartTimeout: 10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := launcher.Launch(ctx)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close(10 * time.Second)

	if fi, err := os.Lstat(client.Socket()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: err=%v mode=%v", err, fi)
	}

	hello, err := client.Hello(ctx)
	if err != nil || hello.Protocol != vmproto.Version {
		t.Fatalf("Hello: %+v err=%v", hello, err)
	}

	// 3. Boot VM with persistent disk and buildkitd (if present in assetsDir).
	diskPath := filepath.Join(tmpDir, "disk.img")
	var services []string
	if _, err := os.Stat(filepath.Join(assetsDir, "buildkit", "bin", "buildkitd")); err == nil {
		services = []string{guestproto.ServiceBuildkitd}
	}
	spec := vmproto.VMSpec{
		Kernel:    kernel,
		Initrd:    initrd,
		Assets:    assetsDir,
		CPUs:      2,
		MemoryMiB: 2048,
		Disk:      &vmproto.DiskSpec{Path: diskPath, Size: 512 << 20},
		Rosetta:   true,
		Network:   true,
		Services:  services,
	}

	tStart := time.Now()
	st, err := client.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("VM booted + ready in %s (state=%s services=%+v)", time.Since(tStart).Round(time.Millisecond), st.State, st.Services)
	if st.State != vmproto.Running {
		t.Fatalf("state = %v, want running", st.State)
	}

	// Dial buildkitd and verify bidirectional HTTP/2 (gRPC) bytes flow over vsock.
	if len(services) > 0 {
		c, err := client.Dial(ctx, vmproto.DialRequest{Service: guestproto.ServiceBuildkitd})
		if err != nil {
			t.Fatalf("Dial buildkitd: %v", err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		// HTTP/2 client connection preface + empty SETTINGS frame (9-byte header).
		preface := append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), 0, 0, 0, 4, 0, 0, 0, 0, 0)
		if _, err := c.Write(preface); err != nil {
			_ = c.Close()
			t.Fatalf("write HTTP/2 preface to buildkitd: %v", err)
		}
		// BuildKit answers with an HTTP/2 SETTINGS frame (frame type byte 3 == 0x04).
		var hdr [9]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			_ = c.Close()
			t.Fatalf("read HTTP/2 frame header from buildkitd: %v", err)
		}
		if hdr[3] != 0x04 {
			_ = c.Close()
			t.Fatalf("unexpected HTTP/2 frame type %#x from buildkitd (want 0x04 SETTINGS)", hdr[3])
		}
		_ = c.Close()
		t.Logf("dial buildkitd over vsock succeeded (received HTTP/2 SETTINGS frame, payload=%d bytes)", int(hdr[0])<<16|int(hdr[1])<<8|int(hdr[2]))
	}

	// Graceful stop via agent op (idle stop path).
	tStop := time.Now()
	st, err = client.Stop(ctx, vmproto.StopRequest{Timeout: vmproto.Duration(10 * time.Second)})
	if err != nil || st.State != vmproto.Stopped {
		t.Fatalf("graceful Stop: st=%+v err=%v", st, err)
	}
	t.Logf("graceful stop took %s", time.Since(tStop).Round(time.Millisecond))

	// Restart with same disk (verifies persistent disk + console.log rotation) and forced stop.
	tRestart := time.Now()
	st, err = client.Start(ctx, spec)
	if err != nil || st.State != vmproto.Running {
		t.Fatalf("restart: st=%+v err=%v", st, err)
	}
	t.Logf("VM restarted on existing disk in %s", time.Since(tRestart).Round(time.Millisecond))
	if _, err := os.Stat(filepath.Join(stateDir, "console.log.1")); err != nil {
		t.Fatalf("expected rotated console.log.1 after restart: %v", err)
	}
	st, err = client.Stop(ctx, vmproto.StopRequest{Force: true})
	if err != nil || st.State != vmproto.Stopped {
		t.Fatalf("forced Stop: st=%+v err=%v", st, err)
	}

	// Close helper cleanly.
	if err := client.Close(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 4. Stdin lease close exits helper automatically.
	leaseCmd := exec.Command(binA, vmhelper.CmdServe, "-dir", stateDir)
	leaseIn, err := leaseCmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	leaseOut, err := leaseCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leaseCmd.Start(); err != nil {
		t.Fatal(err)
	}
	var ready vmproto.Ready
	if err := json.NewDecoder(leaseOut).Decode(&ready); err != nil || !ready.Ready {
		_ = leaseCmd.Process.Kill()
		t.Fatalf("lease ready: err=%v ready=%+v", err, ready)
	}
	_ = leaseIn.Close()
	waitCh := make(chan error, 1)
	go func() { waitCh <- leaseCmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("helper after lease close exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = leaseCmd.Process.Kill()
		t.Fatal("helper did not exit after stdin lease close")
	}

	// 5. SIGKILL of helper takes down the VZ XPC VM process.
	baseXPC := countVZXPC()
	c2, err := launcher.Launch(ctx)
	if err != nil {
		t.Fatalf("Launch(2): %v", err)
	}
	h2, err := c2.Hello(ctx)
	if err != nil {
		_ = c2.Close(time.Second)
		t.Fatalf("Hello(2): %v", err)
	}
	if _, err := c2.Start(ctx, spec); err != nil {
		_ = c2.Close(time.Second)
		t.Fatalf("Start(2): %v", err)
	}
	duringXPC := countVZXPC()
	proc, err := os.FindProcess(h2.PID)
	if err != nil {
		t.Fatalf("FindProcess %d: %v", h2.PID, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("SIGKILL helper %d: %v", h2.PID, err)
	}
	select {
	case <-c2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client.Done not closed after SIGKILL")
	}
	if c2.ExitErr() == nil {
		t.Fatal("expected non-nil ExitErr after SIGKILL")
	}
	// Verify VZ XPC VM process disappears promptly after SIGKILL.
	var afterXPC int
	deadline := time.Now().Add(5 * time.Second)
	for {
		afterXPC = countVZXPC()
		if afterXPC <= baseXPC {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("VZ XPC count stayed at %d after SIGKILL (baseline=%d during=%d)", afterXPC, baseXPC, duringXPC)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("SIGKILL VZ XPC count: baseline=%d during=%d after=%d", baseXPC, duringXPC, afterXPC)
	_ = c2.Close(time.Second)
}

func countVZXPC() int {
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

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}
