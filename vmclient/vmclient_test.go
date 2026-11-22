package vmclient_test

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

// TestHelperProcess is not a real test; TestClient* re-executes the test
// binary with GVX_FAKE_HELPER=1 to act as a real child helper process.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GVX_FAKE_HELPER") != "1" {
		return
	}
	// Strip everything up to "--".
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	fs := flag.NewFlagSet("fake-helper", flag.ExitOnError)
	dir := fs.String("dir", "", "")
	_ = fs.Parse(args)

	sock := filepath.Join(*dir, vmhelper.SocketName)
	// Refuse a live socket, replace a stale one.
	if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		_ = c.Close()
		fmt.Fprintf(os.Stderr, "another process is already listening on %s\n", sock)
		os.Exit(1)
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	_ = os.Chmod(sock, 0o600)
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sock)
	}()

	fmt.Fprintln(os.Stderr, "fake helper started")
	_ = vmproto.WriteLine(os.Stdout, vmproto.Ready{
		Ready:    true,
		PID:      os.Getpid(),
		Protocol: vmproto.Version,
		Socket:   sock,
	})

	done := make(chan struct{})
	var closeOnce sync.Once
	shutdown := func() {
		closeOnce.Do(func() {
			close(done)
			_ = ln.Close()
		})
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		shutdown()
	}()

	var mu sync.Mutex
	state := vmproto.Stopped

	for {
		conn, err := ln.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			var req vmproto.Request
			if err := vmproto.ReadLine(br, &req); err != nil {
				return
			}
			switch req.Op {
			case vmproto.OpHello:
				_ = vmproto.WriteLine(c, vmproto.Response{
					OK: true,
					Hello: &vmproto.Hello{
						Protocol:    vmproto.Version,
						MinProtocol: vmproto.Version,
						Helper:      "fake-proc",
						PID:         os.Getpid(),
					},
				})
			case vmproto.OpStatus:
				mu.Lock()
				st := vmproto.Status{State: state, Since: time.Now().UTC()}
				mu.Unlock()
				_ = vmproto.WriteLine(c, vmproto.Response{OK: true, Status: &st})
			case vmproto.OpStart:
				mu.Lock()
				state = vmproto.Running
				st := vmproto.Status{State: state, Since: time.Now().UTC(), Spec: &req.Start.Spec}
				mu.Unlock()
				_ = vmproto.WriteLine(c, vmproto.Response{OK: true, Status: &st})
			case vmproto.OpStop:
				mu.Lock()
				state = vmproto.Stopped
				st := vmproto.Status{State: state, Since: time.Now().UTC()}
				mu.Unlock()
				_ = vmproto.WriteLine(c, vmproto.Response{OK: true, Status: &st})
			case vmproto.OpDial:
				_ = vmproto.WriteLine(c, vmproto.Response{OK: true})
				_, _ = io.Copy(c, br)
			case vmproto.OpEvents:
				_ = vmproto.WriteLine(c, vmproto.Response{OK: true})
				mu.Lock()
				st := vmproto.Status{State: state, Since: time.Now().UTC()}
				mu.Unlock()
				_ = vmproto.WriteLine(c, vmproto.Event{Time: time.Now().UTC(), Status: st})
				<-done
			case vmproto.OpShutdown:
				mu.Lock()
				state = vmproto.Stopped
				st := vmproto.Status{State: state, Since: time.Now().UTC()}
				mu.Unlock()
				_ = vmproto.WriteLine(c, vmproto.Response{OK: true, Status: &st})
				shutdown()
			default:
				_ = vmproto.WriteLine(c, vmproto.Response{OK: false, Code: vmproto.CodeUnknownOp})
			}
		}(conn)
	}
}

func fakeLauncher(t *testing.T, dir string, logFn func(string)) vmclient.Launcher {
	t.Helper()
	t.Setenv("GVX_FAKE_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return vmclient.Launcher{
		Path:         exe,
		Args:         []string{"-test.run=^TestHelperProcess$", "--"},
		Dir:          dir,
		Log:          logFn,
		StartTimeout: 5 * time.Second,
	}
}

func TestClientLaunchOpsAndClose(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvxd-vc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Place a stale socket in dir to ensure Launch replaces it cleanly.
	staleSock := filepath.Join(dir, vmhelper.SocketName)
	staleLn, err := net.Listen("unix", staleSock)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := staleLn.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	staleLn.Close()

	var logsMu sync.Mutex
	var logs []string
	l := fakeLauncher(t, dir, func(line string) {
		logsMu.Lock()
		logs = append(logs, line)
		logsMu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := l.Launch(ctx)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close(2 * time.Second)

	if client.Socket() != staleSock {
		t.Fatalf("Socket() = %q, want %q", client.Socket(), staleSock)
	}

	// Launching a second helper against the live socket must fail.
	if c2, err := l.Launch(ctx); err == nil {
		_ = c2.Close(time.Second)
		t.Fatal("second Launch on live socket succeeded, want error")
	}

	hello, err := client.Hello(ctx)
	if err != nil || hello.Helper != "fake-proc" || hello.Protocol != vmproto.Version {
		t.Fatalf("Hello = %+v, err=%v", hello, err)
	}

	st, err := client.Status(ctx)
	if err != nil || st.State != vmproto.Stopped {
		t.Fatalf("Status = %+v, err=%v", st, err)
	}

	st, err = client.Start(ctx, vmproto.VMSpec{Kernel: "/k", CPUs: 2, MemoryMiB: 1024})
	if err != nil || st.State != vmproto.Running {
		t.Fatalf("Start = %+v, err=%v", st, err)
	}

	// Dial echo + CloseWrite half-close test.
	conn, err := client.Dial(ctx, vmproto.DialRequest{Service: "buildkitd"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		_ = conn.Close()
		t.Fatal("Dial conn does not implement CloseWrite()")
	}
	if _, err := conn.Write([]byte("ping-dial")); err != nil {
		t.Fatal(err)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	gotAll, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll after CloseWrite: %v", err)
	}
	if string(gotAll) != "ping-dial" {
		t.Fatalf("Dial echo after CloseWrite = %q, want %q", gotAll, "ping-dial")
	}
	_ = conn.Close()

	// Events test.
	evCtx, evCancel := context.WithCancel(ctx)
	gotEvent := make(chan vmproto.State, 1)
	go func() {
		_ = client.Events(evCtx, func(ev vmproto.Event) {
			select {
			case gotEvent <- ev.Status.State:
			default:
			}
		})
	}()
	select {
	case s := <-gotEvent:
		if s != vmproto.Running {
			t.Fatalf("event state = %v, want running", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	evCancel()

	// Stop.
	st, err = client.Stop(ctx, vmproto.StopRequest{})
	if err != nil || st.State != vmproto.Stopped {
		t.Fatalf("Stop = %+v, err=%v", st, err)
	}

	// Close shuts helper down cleanly and ExitErr() is nil.
	if err := client.Close(2 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after Close")
	}
	if err := client.ExitErr(); err != nil {
		t.Fatalf("ExitErr() = %v, want nil for requested Close", err)
	}

	logsMu.Lock()
	joined := strings.Join(logs, "\n")
	logsMu.Unlock()
	if !strings.Contains(joined, "fake helper started") {
		t.Fatalf("stderr log missing expected line: %q", joined)
	}
}

func TestClientUnexpectedCrashSetsExitErr(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gvxd-vck-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	l := fakeLauncher(t, dir, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := l.Launch(ctx)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer client.Close(time.Second)

	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	proc, err := os.FindProcess(hello.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill helper pid %d: %v", hello.PID, err)
	}

	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after helper kill")
	}
	if client.ExitErr() == nil {
		t.Fatal("ExitErr() = nil after unexpected kill, want non-nil error")
	}
}

func TestClientLeaseCloseExitsHelper(t *testing.T) {
	// Verify the real vmhelper.Main serve subcommand exits when its stdin lease
	// closes, without ever receiving OpShutdown.
	dir, err := os.MkdirTemp("/tmp", "gvxd-vcl-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	t.Setenv("GVX_REAL_SERVE_TEST", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	l := vmclient.Launcher{
		Path:         exe,
		Args:         []string{"-test.run=^TestRealServeLeaseProcess$", "--"},
		Dir:          dir,
		StartTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := l.Launch(ctx)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := client.Hello(ctx); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if err := client.Close(2 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-client.Done()
	if client.ExitErr() != nil {
		t.Fatalf("ExitErr = %v, want nil", client.ExitErr())
	}
}

func TestRealServeLeaseProcess(t *testing.T) {
	if os.Getenv("GVX_REAL_SERVE_TEST") != "1" {
		return
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Exit(vmhelper.Main(append([]string{vmhelper.CmdServe}, args...)))
}
