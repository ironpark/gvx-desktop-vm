package vmhelper

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop-vm/vz"
)

// fakeVM simulates a Virtualization.framework VM and an in-process guestproto
// agent over net.Pipe().
type fakeVM struct {
	cfg vz.Config

	startDelay    time.Duration
	startErr      error
	ignoreStop    bool // ignore guest OpShutdown so helper must fall back to Stop()
	services      map[string]bool
	diskMounted   atomic.Bool
	diskFormatted atomic.Bool
	stopCalled    atomic.Bool
	shutdownRecv  atomic.Bool
	closed        atomic.Bool

	mu      sync.Mutex
	done    chan struct{}
	doneErr error
	once    sync.Once
}

func newFakeVM(cfg vz.Config) *fakeVM {
	return &fakeVM{
		cfg:      cfg,
		services: map[string]bool{guestproto.ServiceBuildkitd: true},
		done:     make(chan struct{}),
	}
}

func (f *fakeVM) Start(ctx context.Context) error {
	if f.startDelay > 0 {
		select {
		case <-time.After(f.startDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.startErr != nil {
		return f.startErr
	}
	if f.cfg.Console != nil {
		_, _ = f.cfg.Console.Write([]byte("fake guest boot\n"))
	}
	return nil
}

func (f *fakeVM) Stop(ctx context.Context) error {
	f.stopCalled.Store(true)
	f.finish(nil)
	return nil
}

func (f *fakeVM) Done() <-chan struct{} { return f.done }

func (f *fakeVM) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.doneErr
}

func (f *fakeVM) Close() error {
	f.closed.Store(true)
	f.finish(nil)
	return nil
}

func (f *fakeVM) finish(err error) {
	f.once.Do(func() {
		f.mu.Lock()
		f.doneErr = err
		f.mu.Unlock()
		close(f.done)
	})
}

func (f *fakeVM) Connect(ctx context.Context, port uint32) (net.Conn, error) {
	if port != guestproto.AgentPort {
		return nil, errors.New("connection refused")
	}
	select {
	case <-f.done:
		return nil, errors.New("vm stopped")
	default:
	}
	client, server := net.Pipe()
	go f.serveAgent(server)
	return client, nil
}

func (f *fakeVM) serveAgent(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	var req guestproto.Request
	if err := vmproto.ReadLine(br, &req); err != nil {
		return
	}
	switch req.Op {
	case guestproto.OpHello:
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK: true,
			Hello: &guestproto.Hello{
				Protocol:    guestproto.Version,
				MinProtocol: guestproto.Version,
				Agent:       "fake-1",
				Kernel:      "6.18.0-fake",
				Machine:     "aarch64",
				Rosetta:     f.cfg.Rosetta,
				Uptime:      0.1,
			},
		})
	case guestproto.OpDiskMount:
		f.diskMounted.Store(true)
		if req.Disk != nil && req.Disk.Format {
			f.diskFormatted.Store(true)
		}
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:   true,
			Disk: &guestproto.DiskStatus{Formatted: true, Size: 64 << 20, Free: 60 << 20, FSType: "ext4"},
		})
	case guestproto.OpServiceStart:
		if req.Service == nil || !f.services[req.Service.Name] {
			_ = vmproto.WriteLine(conn, guestproto.Response{
				OK:    false,
				Code:  guestproto.CodeNoService,
				Error: "unknown service",
			})
			return
		}
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:       true,
			Services: []guestproto.ServiceStatus{{Name: req.Service.Name, State: "running", PID: 42}},
		})
	case guestproto.OpServiceStop:
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:       true,
			Services: []guestproto.ServiceStatus{{Name: req.Service.Name, State: "stopped"}},
		})
	case guestproto.OpShutdown:
		f.shutdownRecv.Store(true)
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
		if !f.ignoreStop {
			go func() {
				time.Sleep(15 * time.Millisecond)
				f.finish(nil)
			}()
		}
	case guestproto.OpDial:
		if req.Dial == nil || (req.Dial.Service != "" && !f.services[req.Dial.Service]) {
			_ = vmproto.WriteLine(conn, guestproto.Response{
				OK:    false,
				Code:  guestproto.CodeNoService,
				Error: "no such service",
			})
			return
		}
		if err := vmproto.WriteLine(conn, guestproto.Response{OK: true}); err != nil {
			return
		}
		// Simple echo service over the forwarded stream.
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	default:
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeUnknownOp,
			Error: "unknown op",
		})
	}
}

func startTestServer(t *testing.T, factory vmFactory) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gvxd-vh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, SocketName)
	ln, err := listenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(serverOptions{
		dir:           dir,
		helperVersion: "test-1",
		newVM:         factory,
		rosettaAvail:  func() vz.Availability { return vz.RosettaInstalled },
		agentReady:    2 * time.Second,
		agentPoll:     5 * time.Millisecond,
		serviceWait:   2 * time.Second,
		logf:          func(format string, args ...any) {},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		srv.Shutdown(vmproto.StopRequest{Force: true})
		<-done
	})
	return srv, sock
}

func sendReq(t *testing.T, sock string, req vmproto.Request) vmproto.Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	if err := vmproto.WriteLine(conn, req); err != nil {
		t.Fatalf("write req: %v", err)
	}
	var resp vmproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(conn), &resp); err != nil {
		t.Fatalf("read resp: %v", err)
	}
	return resp
}

func TestServerOpsAndProtocolErrors(t *testing.T) {
	var created atomic.Int32
	var lastVM *fakeVM
	var vmMu sync.Mutex

	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		created.Add(1)
		f := newFakeVM(cfg)
		vmMu.Lock()
		lastVM = f
		vmMu.Unlock()
		return f, nil
	})

	// Socket permissions must be 0600 on Unix.
	if runtime.GOOS != "windows" {
		fi, err := os.Lstat(sock)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("socket mode = %04o, want 0600", mode)
		}
	}

	// Bad version.
	resp := sendReq(t, sock, vmproto.Request{V: 99, Op: vmproto.OpHello})
	if resp.OK || resp.Code != vmproto.CodeBadVersion {
		t.Fatalf("bad version resp = %+v, want bad-version", resp)
	}

	// Unknown op.
	resp = sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: "bogus"})
	if resp.OK || resp.Code != vmproto.CodeUnknownOp {
		t.Fatalf("unknown op resp = %+v, want unknown-op", resp)
	}

	// Hello.
	resp = sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpHello})
	if !resp.OK || resp.Hello == nil || resp.Hello.Protocol != vmproto.Version || resp.Hello.Helper != "test-1" {
		t.Fatalf("hello resp = %+v", resp)
	}

	// Status while stopped.
	resp = sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpStatus})
	if !resp.OK || resp.Status == nil || resp.Status.State != vmproto.Stopped {
		t.Fatalf("initial status = %+v", resp)
	}

	// Dial while stopped -> not-running.
	resp = sendReq(t, sock, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpDial,
		Dial: &vmproto.DialRequest{Service: guestproto.ServiceBuildkitd},
	})
	if resp.OK || resp.Code != vmproto.CodeNotRunning {
		t.Fatalf("dial stopped resp = %+v, want not-running", resp)
	}

	// Start with disk + buildkitd service.
	diskPath := filepath.Join(filepath.Dir(sock), "disk.img")
	spec := vmproto.VMSpec{
		Kernel:    "/fake/Image",
		Initrd:    "/fake/initramfs.gz",
		Assets:    "/fake/assets",
		CPUs:      2,
		MemoryMiB: 2048,
		Disk:      &vmproto.DiskSpec{Path: diskPath, Size: 16 << 20},
		Rosetta:   true,
		Network:   true,
		Services:  []string{guestproto.ServiceBuildkitd},
	}
	resp = sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if !resp.OK || resp.Status == nil || resp.Status.State != vmproto.Running {
		t.Fatalf("start resp = %+v", resp)
	}
	if len(resp.Status.Services) != 1 || resp.Status.Services[0].Name != guestproto.ServiceBuildkitd {
		t.Fatalf("services = %+v", resp.Status.Services)
	}
	// Sparse disk should have been created.
	if fi, err := os.Stat(diskPath); err != nil || fi.Size() != 16<<20 {
		t.Fatalf("disk stat: err=%v, fi=%v", err, fi)
	}

	vmMu.Lock()
	vm := lastVM
	vmMu.Unlock()
	if !vm.diskMounted.Load() || !vm.diskFormatted.Load() {
		t.Fatal("expected disk-mount with format=true")
	}
	if !strings.Contains(vm.cfg.CommandLine, "gvx.assets=assets") ||
		!strings.Contains(vm.cfg.CommandLine, "gvx.rosetta=rosetta") ||
		!strings.Contains(vm.cfg.CommandLine, "gvx.net=1") {
		t.Fatalf("unexpected cmdline: %q", vm.cfg.CommandLine)
	}

	// Start again with identical spec succeeds without making a second VM.
	resp = sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if !resp.OK || created.Load() != 1 {
		t.Fatalf("idempotent start failed: resp=%+v created=%d", resp, created.Load())
	}

	// Start with different spec answers CodeBusy.
	otherSpec := spec
	otherSpec.MemoryMiB = 4096
	resp = sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: otherSpec},
	})
	if resp.OK || resp.Code != vmproto.CodeBusy {
		t.Fatalf("different spec start = %+v, want busy", resp)
	}

	// Dial unknown service answers CodeNoService.
	resp = sendReq(t, sock, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpDial,
		Dial: &vmproto.DialRequest{Service: "missing"},
	})
	if resp.OK || resp.Code != vmproto.CodeNoService {
		t.Fatalf("dial missing service = %+v, want no-service", resp)
	}

	// Dial buildkitd forwards bytes both ways.
	dialConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := vmproto.WriteLine(dialConn, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpDial,
		Dial: &vmproto.DialRequest{Service: guestproto.ServiceBuildkitd},
	}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(dialConn)
	var dresp vmproto.Response
	if err := vmproto.ReadLine(br, &dresp); err != nil || !dresp.OK {
		t.Fatalf("dial resp = %+v, err=%v", dresp, err)
	}
	msg := "hello-over-vsock-forward"
	if _, err := dialConn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
	_ = dialConn.Close()

	// Graceful stop uses agent shutdown (not forced Stop).
	resp = sendReq(t, sock, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpStop,
		Stop: &vmproto.StopRequest{Timeout: vmproto.Duration(2 * time.Second)},
	})
	if !resp.OK || resp.Status == nil || resp.Status.State != vmproto.Stopped {
		t.Fatalf("stop resp = %+v", resp)
	}
	if !vm.shutdownRecv.Load() {
		t.Fatal("expected graceful shutdown op")
	}
	if vm.stopCalled.Load() {
		t.Fatal("unexpected force Stop() after clean graceful shutdown")
	}
}

func TestServerSingleFlightStartAndEvents(t *testing.T) {
	var created atomic.Int32
	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		created.Add(1)
		f := newFakeVM(cfg)
		f.startDelay = 80 * time.Millisecond
		return f, nil
	})

	// Subscribe to events before start.
	evConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer evConn.Close()
	if err := vmproto.WriteLine(evConn, vmproto.Request{V: vmproto.Version, Op: vmproto.OpEvents}); err != nil {
		t.Fatal(err)
	}
	evReader := bufio.NewReader(evConn)
	var evResp vmproto.Response
	if err := vmproto.ReadLine(evReader, &evResp); err != nil || !evResp.OK {
		t.Fatalf("events handshake: resp=%+v err=%v", evResp, err)
	}
	eventsCh := make(chan vmproto.State, 16)
	go func() {
		for {
			var ev vmproto.Event
			if err := vmproto.ReadLine(evReader, &ev); err != nil {
				return
			}
			eventsCh <- ev.Status.State
		}
	}()

	// Initial state should arrive first.
	select {
	case st := <-eventsCh:
		if st != vmproto.Stopped {
			t.Fatalf("first event state = %v, want stopped", st)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial event")
	}

	spec := vmproto.VMSpec{
		Kernel:    "/fake/Image",
		CPUs:      2,
		MemoryMiB: 1024,
	}

	// Launch 5 concurrent starts with the same spec -> only 1 VM created.
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := sendReq(t, sock, vmproto.Request{
				V:     vmproto.Version,
				Op:    vmproto.OpStart,
				Start: &vmproto.StartRequest{Spec: spec},
			})
			if !r.OK || r.Status.State != vmproto.Running {
				errs <- errors.New("start failed: " + r.Error)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if created.Load() != 1 {
		t.Fatalf("created %d VMs, want 1", created.Load())
	}

	// Verify Starting -> Running events arrived.
	var seenStarting, seenRunning bool
	deadline := time.After(2 * time.Second)
	for !(seenStarting && seenRunning) {
		select {
		case st := <-eventsCh:
			if st == vmproto.Starting {
				seenStarting = true
			}
			if st == vmproto.Running {
				seenRunning = true
			}
		case <-deadline:
			t.Fatalf("events missing: starting=%v running=%v", seenStarting, seenRunning)
		}
	}
}

func TestServerStopUnderLoadAndForceFallback(t *testing.T) {
	var vm *fakeVM
	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		vm = newFakeVM(cfg)
		vm.ignoreStop = true // force timeout fallback to vm.Stop()
		return vm, nil
	})

	spec := vmproto.VMSpec{
		Kernel:    "/fake/Image",
		CPUs:      2,
		MemoryMiB: 1024,
		Services:  []string{guestproto.ServiceBuildkitd},
	}
	resp := sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if !resp.OK {
		t.Fatalf("start: %+v", resp)
	}

	// Open an active dial stream while stop runs.
	dialConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer dialConn.Close()
	if err := vmproto.WriteLine(dialConn, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpDial,
		Dial: &vmproto.DialRequest{Service: guestproto.ServiceBuildkitd},
	}); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(dialConn)
	var dresp vmproto.Response
	if err := vmproto.ReadLine(br, &dresp); err != nil || !dresp.OK {
		t.Fatalf("dial: %+v err=%v", dresp, err)
	}

	// Stop with short graceful timeout -> falls back to force Stop, and closes active dial.
	resp = sendReq(t, sock, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpStop,
		Stop: &vmproto.StopRequest{Timeout: vmproto.Duration(80 * time.Millisecond)},
	})
	if !resp.OK || resp.Status.State != vmproto.Stopped {
		t.Fatalf("stop resp = %+v", resp)
	}
	if !vm.shutdownRecv.Load() || !vm.stopCalled.Load() {
		t.Fatalf("expected both shutdown attempt (%v) and force fallback (%v)", vm.shutdownRecv.Load(), vm.stopCalled.Load())
	}

	// Active dial connection should see EOF/error promptly.
	_ = dialConn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 16)
	if _, err := br.Read(buf); err == nil {
		t.Fatal("expected active dial connection to close on VM stop")
	}
}

func TestListenUnixSafety(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvxd-lu-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Regular file must not be removed.
	reg := filepath.Join(dir, "reg.sock")
	if err := os.WriteFile(reg, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(reg); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("listenUnix on regular file = %v, want not-a-socket error", err)
	}

	// Live socket must be refused.
	live := filepath.Join(dir, "live.sock")
	ln1, err := listenUnix(live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	if _, err := listenUnix(live); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("listenUnix on live socket = %v, want already-listening error", err)
	}

	// Closed (stale) socket must be replaced cleanly.
	ln1.Close()
	ln2, err := listenUnix(live)
	if err != nil {
		t.Fatalf("listenUnix replacing stale socket: %v", err)
	}
	ln2.Close()
}

func TestConsoleSinkRotationAndCap(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvxd-con-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// First sink: write > 1 MiB and verify file is capped at 1 MiB without blocking.
	s1, err := openConsoleSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("a", 64*1024)
	for range 20 { // 1.25 MiB
		if _, err := s1.File().WriteString(chunk); err != nil {
			t.Fatalf("write console: %v", err)
		}
	}
	s1.CloseWriter()
	<-s1.done

	fi, err := os.Stat(s1.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != maxConsoleBytes {
		t.Fatalf("console.log size = %d, want %d", fi.Size(), maxConsoleBytes)
	}

	// Second sink rotates console.log to console.log.1 and starts fresh.
	s2, err := openConsoleSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s2.File().WriteString("second-boot\n")
	s2.CloseWriter()
	<-s2.done

	if _, err := os.Stat(s2.Path() + ".1"); err != nil {
		t.Fatalf("rotated console.log.1 missing: %v", err)
	}
	b, err := os.ReadFile(s2.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "second-boot\n" {
		t.Fatalf("new console.log = %q", string(b))
	}
}

func TestServerUnexpectedVMExitAndRestart(t *testing.T) {
	var vmMu sync.Mutex
	var currentVM *fakeVM
	var count atomic.Int32

	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		count.Add(1)
		f := newFakeVM(cfg)
		vmMu.Lock()
		currentVM = f
		vmMu.Unlock()
		return f, nil
	})

	spec := vmproto.VMSpec{
		Kernel:    "/fake/Image",
		CPUs:      2,
		MemoryMiB: 1024,
	}
	resp := sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if !resp.OK || resp.Status.State != vmproto.Running {
		t.Fatalf("start 1: %+v", resp)
	}

	// Crash the VM unexpectedly.
	vmMu.Lock()
	vm1 := currentVM
	vmMu.Unlock()
	vm1.finish(errors.New("simulated kernel panic"))

	// Wait for status to transition to Failed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpStatus})
		if st.OK && st.Status.State == vmproto.Failed {
			if !strings.Contains(st.Status.Err, "simulated kernel panic") {
				t.Fatalf("unexpected err: %q", st.Status.Err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Failed status, got %+v", st.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Next Start recovers cleanly.
	resp = sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if !resp.OK || resp.Status.State != vmproto.Running || count.Load() != 2 {
		t.Fatalf("restart failed: resp=%+v count=%d", resp, count.Load())
	}
}

func TestServerOpShutdown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvxd-sd-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	sock := filepath.Join(dir, SocketName)
	ln, err := listenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(serverOptions{
		dir:          dir,
		newVM:        func(cfg vz.Config) (vmInstance, error) { return newFakeVM(cfg), nil },
		rosettaAvail: func() vz.Availability { return vz.RosettaInstalled },
		logf:         func(string, ...any) {},
	})
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()

	resp := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpShutdown})
	if !resp.OK || resp.Status == nil || resp.Status.State != vmproto.Stopped {
		t.Fatalf("shutdown resp = %+v", resp)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error after OpShutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after OpShutdown")
	}
}

func TestServerCancelDuringStartCallsStopBeforeClose(t *testing.T) {
	started := make(chan struct{})
	var vmMu sync.Mutex
	var createdVM *fakeVM

	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		f := newFakeVM(cfg)
		f.startDelay = 5 * time.Second
		vmMu.Lock()
		createdVM = f
		vmMu.Unlock()
		close(started)
		return f, nil
	})

	startDone := make(chan vmproto.Response, 1)
	go func() {
		startDone <- sendReq(t, sock, vmproto.Request{
			V:  vmproto.Version,
			Op: vmproto.OpStart,
			Start: &vmproto.StartRequest{Spec: vmproto.VMSpec{
				Kernel:    "/fake/Image",
				CPUs:      2,
				MemoryMiB: 1024,
			}},
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fake VM was never created")
	}
	// Give vm.Start(ctx) a moment to enter startDelay, then cancel via OpStop.
	time.Sleep(30 * time.Millisecond)

	stopResp := sendReq(t, sock, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpStop,
		Stop: &vmproto.StopRequest{Force: true},
	})
	if !stopResp.OK || stopResp.Status.State != vmproto.Stopped {
		t.Fatalf("stop during start = %+v", stopResp)
	}

	select {
	case r := <-startDone:
		if r.OK {
			t.Fatalf("expected canceled start to fail, got %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled start did not return")
	}

	vmMu.Lock()
	vm := createdVM
	vmMu.Unlock()
	if !vm.stopCalled.Load() || !vm.closed.Load() {
		t.Fatalf("expected abortVM to call Stop (%v) and Close (%v) on canceled Start", vm.stopCalled.Load(), vm.closed.Load())
	}
}

func TestServerDialHalfCloseAndFullClose(t *testing.T) {
	// Custom fake VM whose "reqresp" service reads a line, waits briefly, writes
	// a reply, and closes (testing client CloseWrite), and whose "silent" service
	// blocks reading forever until closed (testing client full Close).
	silentUnblocked := make(chan struct{})
	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		return &halfCloseFakeVM{
			fakeVM:          newFakeVM(cfg),
			silentUnblocked: silentUnblocked,
		}, nil
	})

	resp := sendReq(t, sock, vmproto.Request{
		V:  vmproto.Version,
		Op: vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: vmproto.VMSpec{
			Kernel:    "/fake/Image",
			CPUs:      2,
			MemoryMiB: 1024,
		}},
	})
	if !resp.OK {
		t.Fatalf("start: %+v", resp)
	}

	// 1. Half-close (CloseWrite) must still receive the guest's delayed reply.
	c1, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	if err := vmproto.WriteLine(c1, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpDial,
		Dial: &vmproto.DialRequest{Service: "reqresp"},
	}); err != nil {
		t.Fatal(err)
	}
	br1 := bufio.NewReader(c1)
	var dresp vmproto.Response
	if err := vmproto.ReadLine(br1, &dresp); err != nil || !dresp.OK {
		t.Fatalf("dial reqresp: %+v err=%v", dresp, err)
	}
	if _, err := c1.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if cw, ok := c1.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	}
	reply, err := io.ReadAll(br1)
	if err != nil {
		t.Fatalf("ReadAll after CloseWrite: %v", err)
	}
	if string(reply) != "reply:ping\n" {
		t.Fatalf("reply = %q, want %q", reply, "reply:ping\n")
	}

	// 2. Full Close on darwin/unix must unblock a silent guest stream promptly.
	if runtime.GOOS == "darwin" {
		c2, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := vmproto.WriteLine(c2, vmproto.Request{
			V:    vmproto.Version,
			Op:   vmproto.OpDial,
			Dial: &vmproto.DialRequest{Service: "silent"},
		}); err != nil {
			t.Fatal(err)
		}
		br2 := bufio.NewReader(c2)
		var dresp2 vmproto.Response
		if err := vmproto.ReadLine(br2, &dresp2); err != nil || !dresp2.OK {
			t.Fatalf("dial silent: %+v err=%v", dresp2, err)
		}
		_ = c2.Close()
		select {
		case <-silentUnblocked:
		case <-time.After(2 * time.Second):
			t.Fatal("silent guest dial stream did not unblock after client full Close")
		}
	}
}

type halfCloseFakeVM struct {
	*fakeVM
	silentUnblocked chan struct{}
}

func (h *halfCloseFakeVM) Connect(ctx context.Context, port uint32) (net.Conn, error) {
	if port != guestproto.AgentPort {
		return nil, errors.New("connection refused")
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		br := bufio.NewReader(server)
		var req guestproto.Request
		if err := vmproto.ReadLine(br, &req); err != nil {
			return
		}
		if req.Op != guestproto.OpDial || req.Dial == nil {
			// Delegate standard control ops to fakeVM's protocol.
			switch req.Op {
			case guestproto.OpHello:
				_ = vmproto.WriteLine(server, guestproto.Response{
					OK:    true,
					Hello: &guestproto.Hello{Protocol: guestproto.Version, MinProtocol: guestproto.Version, Agent: "fake"},
				})
			case guestproto.OpShutdown:
				_ = vmproto.WriteLine(server, guestproto.Response{OK: true})
				h.finish(nil)
			}
			return
		}
		switch req.Dial.Service {
		case "reqresp":
			if err := vmproto.WriteLine(server, guestproto.Response{OK: true}); err != nil {
				return
			}
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			time.Sleep(40 * time.Millisecond)
			_, _ = server.Write([]byte("reply:" + line))
		case "silent":
			if err := vmproto.WriteLine(server, guestproto.Response{OK: true}); err != nil {
				return
			}
			defer close(h.silentUnblocked)
			_, _ = io.Copy(io.Discard, br)
		}
	}()
	return client, nil
}
