package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

type fakeProc struct {
	pid         int
	done        chan error
	terminated  atomic.Bool
	killed      atomic.Bool
	logTail     string
	onTerminate func()
	onKill      func()
}

func newFakeProc(pid int) *fakeProc {
	return &fakeProc{pid: pid, done: make(chan error, 1)}
}

func (p *fakeProc) PID() int           { return p.pid }
func (p *fakeProc) Done() <-chan error { return p.done }
func (p *fakeProc) LogTail() string    { return p.logTail }

func (p *fakeProc) Terminate() error {
	p.terminated.Store(true)
	if p.onTerminate != nil {
		p.onTerminate()
	}
	return nil
}

func (p *fakeProc) Kill() error {
	p.killed.Store(true)
	if p.onKill != nil {
		p.onKill()
	}
	return nil
}

func (p *fakeProc) finish(err error) {
	p.done <- err
	close(p.done)
}

type fakeOps struct {
	mu           sync.Mutex
	info         hostInfo
	mountDiskFn  func(format bool) (guestproto.DiskStatus, error)
	unmountErr   error
	startSvcFn   func(name string) (serviceProc, error)
	dialFn       func(network, addr string) (net.Conn, error)
	syncCount    atomic.Int32
	unmountCount atomic.Int32
	powerOffCh   chan struct{}
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		info: hostInfo{
			Kernel:  "6.18.64-0-virt",
			Machine: "aarch64",
			Rosetta: true,
			Uptime:  1.25,
		},
		powerOffCh: make(chan struct{}, 1),
	}
}

func (f *fakeOps) HostInfo() hostInfo { return f.info }

func (f *fakeOps) MountDisk(format bool) (guestproto.DiskStatus, error) {
	if f.mountDiskFn != nil {
		return f.mountDiskFn(format)
	}
	return guestproto.DiskStatus{Formatted: format, Size: 64 << 30, Free: 60 << 30, FSType: "ext4"}, nil
}

func (f *fakeOps) UnmountDisk() error {
	f.unmountCount.Add(1)
	return f.unmountErr
}

func (f *fakeOps) Sync() {
	f.syncCount.Add(1)
}

func (f *fakeOps) PowerOff() {
	select {
	case f.powerOffCh <- struct{}{}:
	default:
	}
}

func (f *fakeOps) StartService(name string) (serviceProc, error) {
	if f.startSvcFn != nil {
		return f.startSvcFn(name)
	}
	return nil, errors.New("start not configured")
}

func (f *fakeOps) Dial(network, addr string) (net.Conn, error) {
	if f.dialFn != nil {
		return f.dialFn(network, addr)
	}
	return nil, errors.New("dial refused")
}

func roundTrip(t *testing.T, srv *Server, req guestproto.Request) guestproto.Response {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	go srv.ServeConn(server)

	if err := vmproto.WriteLine(client, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	var resp guestproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(client), &resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

func TestIsAllowedVsockCID(t *testing.T) {
	if !isAllowedVsockCID(2) {
		t.Fatalf("host CID 2 must be allowed")
	}
	for _, cid := range []uint32{0, 1, 3, 4, 0xffffffff} {
		if isAllowedVsockCID(cid) {
			t.Fatalf("peer CID %d must be rejected", cid)
		}
	}
}

func TestHelloAndErrors(t *testing.T) {
	ops := newFakeOps()
	srv := NewServer("1", ops)

	resp := roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpHello})
	if !resp.OK || resp.Hello == nil {
		t.Fatalf("hello failed: %+v", resp)
	}
	if resp.Hello.Protocol != guestproto.Version || resp.Hello.Agent != "1" || !resp.Hello.Rosetta {
		t.Fatalf("unexpected hello: %+v", resp.Hello)
	}
	if resp.Hello.Kernel != "6.18.64-0-virt" || resp.Hello.Machine != "aarch64" {
		t.Fatalf("unexpected kernel/machine: %+v", resp.Hello)
	}

	// Bad version.
	resp = roundTrip(t, srv, guestproto.Request{V: 99, Op: guestproto.OpHello})
	if resp.OK || resp.Code != guestproto.CodeBadVersion {
		t.Fatalf("want bad-version, got %+v", resp)
	}

	// Unknown op.
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: "no-such-op"})
	if resp.OK || resp.Code != guestproto.CodeUnknownOp {
		t.Fatalf("want unknown-op, got %+v", resp)
	}

	// Malformed JSON.
	client, server := net.Pipe()
	defer client.Close()
	go srv.ServeConn(server)
	_, _ = io.WriteString(client, "{bad json\n")
	var badResp guestproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(client), &badResp); err != nil {
		t.Fatalf("read bad json resp: %v", err)
	}
	if badResp.OK || badResp.Code != guestproto.CodeBadRequest {
		t.Fatalf("want bad-request, got %+v", badResp)
	}
}

func TestDiskMount(t *testing.T) {
	ops := newFakeOps()
	mounted := false
	ops.mountDiskFn = func(format bool) (guestproto.DiskStatus, error) {
		if mounted {
			return guestproto.DiskStatus{Formatted: false, Size: 1000, Free: 900, FSType: "ext4"}, nil
		}
		if !format {
			return guestproto.DiskStatus{}, errors.New("unformatted")
		}
		mounted = true
		return guestproto.DiskStatus{Formatted: true, Size: 1000, Free: 950, FSType: "ext4"}, nil
	}
	srv := NewServer("1", ops)

	// Without format on unformatted disk: fails.
	resp := roundTrip(t, srv, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDiskMount,
		Disk: &guestproto.DiskRequest{Format: false},
	})
	if resp.OK || resp.Code != guestproto.CodeFailed {
		t.Fatalf("want failed without format, got %+v", resp)
	}

	// First mount with format=true formats the disk.
	resp = roundTrip(t, srv, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDiskMount,
		Disk: &guestproto.DiskRequest{Format: true},
	})
	if !resp.OK || resp.Disk == nil || !resp.Disk.Formatted || resp.Disk.FSType != "ext4" {
		t.Fatalf("want formatted ext4, got %+v", resp)
	}

	// Second mount is idempotent and does not reformat.
	resp = roundTrip(t, srv, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDiskMount,
		Disk: &guestproto.DiskRequest{Format: true},
	})
	if !resp.OK || resp.Disk == nil || resp.Disk.Formatted {
		t.Fatalf("want mounted without reformat, got %+v", resp)
	}
}

func TestExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	srv := NewServer("1", newFakeOps())

	runExec := func(req *guestproto.ExecRequest, stdin string) (stdout, stderr string, exit guestproto.Exit) {
		t.Helper()
		client, server := net.Pipe()
		defer client.Close()
		go srv.ServeConn(server)

		if err := vmproto.WriteLine(client, guestproto.Request{
			V:    guestproto.Version,
			Op:   guestproto.OpExec,
			Exec: req,
		}); err != nil {
			t.Fatalf("write exec req: %v", err)
		}
		br := bufio.NewReader(client)
		var resp guestproto.Response
		if err := vmproto.ReadLine(br, &resp); err != nil {
			t.Fatalf("read exec resp: %v", err)
		}
		if !resp.OK {
			t.Fatalf("exec not ok: %+v", resp)
		}
		if req.Stdin {
			go func() {
				if len(stdin) > 0 {
					_ = guestproto.WriteFrame(client, guestproto.FrameStdin, []byte(stdin))
				}
				_ = guestproto.WriteFrame(client, guestproto.FrameStdinEOF, nil)
			}()
		}

		var outBuf, errBuf bytes.Buffer
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				t.Fatalf("read frame: %v", err)
			}
			switch kind {
			case guestproto.FrameStdout:
				outBuf.Write(payload)
			case guestproto.FrameStderr:
				errBuf.Write(payload)
			case guestproto.FrameExit:
				if err := json.Unmarshal(payload, &exit); err != nil {
					t.Fatalf("unmarshal exit: %v", err)
				}
				return outBuf.String(), errBuf.String(), exit
			default:
				t.Fatalf("unexpected frame kind %d", kind)
			}
		}
	}

	// Stdout, stderr, env, exit code.
	out, errOut, exit := runExec(&guestproto.ExecRequest{
		Argv: []string{"/bin/sh", "-c", "printf '%s' \"$GREETING\"; printf 'err!' >&2; exit 7"},
		Env:  []string{"GREETING=hello"},
	}, "")
	if out != "hello" || errOut != "err!" || exit.Code != 7 || exit.Error != "" {
		t.Fatalf("got out=%q err=%q exit=%+v", out, errOut, exit)
	}

	// Stdin framing.
	out, _, exit = runExec(&guestproto.ExecRequest{
		Argv:  []string{"/bin/cat"},
		Stdin: true,
	}, "from-stdin\n")
	if out != "from-stdin\n" || exit.Code != 0 {
		t.Fatalf("stdin exec: out=%q exit=%+v", out, exit)
	}

	// Missing command reports exit 127 with error string.
	_, _, exit = runExec(&guestproto.ExecRequest{
		Argv: []string{"/nonexistent-binary-gvx-test"},
	}, "")
	if exit.Code != 127 || exit.Error == "" {
		t.Fatalf("want exit 127 with error, got %+v", exit)
	}

	// Empty argv is CodeBadRequest.
	resp := roundTrip(t, srv, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpExec,
		Exec: &guestproto.ExecRequest{},
	})
	if resp.OK || resp.Code != guestproto.CodeBadRequest {
		t.Fatalf("want bad-request for empty argv, got %+v", resp)
	}
}

func TestFrameWriterChunksLargePayloads(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	fw := frameWriter{mu: &mu, w: &buf, kind: guestproto.FrameStdout}

	big := bytes.Repeat([]byte("a"), guestproto.MaxFrame+1234)
	n, err := fw.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("write big: n=%d err=%v", n, err)
	}

	r := bytes.NewReader(buf.Bytes())
	var got bytes.Buffer
	frames := 0
	for r.Len() > 0 {
		kind, payload, err := guestproto.ReadFrame(r)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if kind != guestproto.FrameStdout {
			t.Fatalf("kind=%d", kind)
		}
		got.Write(payload)
		frames++
	}
	if frames != 2 || !bytes.Equal(got.Bytes(), big) {
		t.Fatalf("frames=%d len=%d", frames, got.Len())
	}
}

func TestDialServiceAndDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 5)
				if _, err := io.ReadFull(c, buf); err == nil {
					_, _ = c.Write([]byte("echo:" + string(buf)))
				}
			}(c)
		}
	}()

	ops := newFakeOps()
	ops.dialFn = func(network, addr string) (net.Conn, error) {
		if network == "unix" && addr == guestproto.BuildkitdSocket {
			return net.Dial("tcp", ln.Addr().String())
		}
		return net.Dial(network, addr)
	}
	srv := NewServer("1", ops)

	// Dial by service name.
	for _, req := range []guestproto.DialRequest{
		{Service: guestproto.ServiceBuildkitd},
		{Network: "tcp", Addr: ln.Addr().String()},
	} {
		client, server := net.Pipe()
		go srv.ServeConn(server)

		if err := vmproto.WriteLine(client, guestproto.Request{
			V:    guestproto.Version,
			Op:   guestproto.OpDial,
			Dial: &req,
		}); err != nil {
			t.Fatalf("write dial req: %v", err)
		}
		br := bufio.NewReader(client)
		var resp guestproto.Response
		if err := vmproto.ReadLine(br, &resp); err != nil {
			t.Fatalf("read dial resp: %v", err)
		}
		if !resp.OK {
			t.Fatalf("dial %+v failed: %+v", req, resp)
		}
		if _, err := client.Write([]byte("hello")); err != nil {
			t.Fatalf("write stream: %v", err)
		}
		reply, err := io.ReadAll(br)
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if string(reply) != "echo:hello" {
			t.Fatalf("dial %+v got %q, want echo:hello", req, reply)
		}
		_ = client.Close()
	}

	// Unknown service.
	resp := roundTrip(t, srv, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDial,
		Dial: &guestproto.DialRequest{Service: "nope"},
	})
	if resp.OK || resp.Code != guestproto.CodeNoService {
		t.Fatalf("want no-service, got %+v", resp)
	}

	// Bad network.
	resp = roundTrip(t, srv, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDial,
		Dial: &guestproto.DialRequest{Network: "udp", Addr: "127.0.0.1:1"},
	})
	if resp.OK || resp.Code != guestproto.CodeBadRequest {
		t.Fatalf("want bad-request, got %+v", resp)
	}
}

func TestServiceLifecycleAndShutdown(t *testing.T) {
	ops := newFakeOps()
	var dialAttempts atomic.Int32
	var proc *fakeProc

	ops.startSvcFn = func(name string) (serviceProc, error) {
		p := newFakeProc(4242)
		p.onTerminate = func() { p.finish(nil) }
		p.onKill = func() { p.finish(errors.New("killed")) }
		proc = p
		return p, nil
	}
	ops.dialFn = func(network, addr string) (net.Conn, error) {
		if dialAttempts.Add(1) < 2 {
			return nil, errors.New("not ready yet")
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	srv := NewServer("1", ops)
	srv.shutdownDelay = time.Millisecond
	srv.services[guestproto.ServiceBuildkitd].pollInterval = 5 * time.Millisecond

	// Initial status is stopped.
	resp := roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpServiceStatus})
	if !resp.OK || len(resp.Services) != 1 || resp.Services[0].State != "stopped" {
		t.Fatalf("initial status: %+v", resp)
	}

	// Start waits until socket accepts.
	resp = roundTrip(t, srv, guestproto.Request{
		V:       guestproto.Version,
		Op:      guestproto.OpServiceStart,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd, Timeout: 2000},
	})
	if !resp.OK || len(resp.Services) != 1 || resp.Services[0].State != "running" || resp.Services[0].PID != 4242 {
		t.Fatalf("service start: %+v", resp)
	}

	// Idempotent second start returns running without spawning again.
	resp = roundTrip(t, srv, guestproto.Request{
		V:       guestproto.Version,
		Op:      guestproto.OpServiceStart,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd},
	})
	if !resp.OK || resp.Services[0].State != "running" {
		t.Fatalf("second start: %+v", resp)
	}

	// Shutdown stops running services, syncs, unmounts, replies OK, then powers off.
	resp = roundTrip(t, srv, guestproto.Request{V: guestproto.Version, Op: guestproto.OpShutdown})
	if !resp.OK {
		t.Fatalf("shutdown failed: %+v", resp)
	}
	if !proc.terminated.Load() {
		t.Fatalf("shutdown did not terminate buildkitd")
	}
	if ops.unmountCount.Load() != 1 {
		t.Fatalf("unmountCount = %d, want 1", ops.unmountCount.Load())
	}
	if ops.syncCount.Load() < 2 {
		t.Fatalf("syncCount = %d, want >= 2", ops.syncCount.Load())
	}
	select {
	case <-ops.powerOffCh:
	case <-time.After(time.Second):
		t.Fatalf("powerOff was not called")
	}
}

func TestServiceStartCrashAndStopKillEscalation(t *testing.T) {
	ops := newFakeOps()
	srv := NewServer("1", ops)
	srv.services[guestproto.ServiceBuildkitd].pollInterval = 5 * time.Millisecond

	// Case 1: process exits before socket is ready.
	ops.startSvcFn = func(name string) (serviceProc, error) {
		p := newFakeProc(100)
		p.logTail = "fatal: bad config"
		go func() {
			time.Sleep(10 * time.Millisecond)
			p.finish(errors.New("exit status 1"))
		}()
		return p, nil
	}
	resp := roundTrip(t, srv, guestproto.Request{
		V:       guestproto.Version,
		Op:      guestproto.OpServiceStart,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd, Timeout: 1000},
	})
	if resp.OK || resp.Code != guestproto.CodeFailed || !strings.Contains(resp.Error, "bad config") {
		t.Fatalf("want failed with log tail, got %+v", resp)
	}

	// Case 2: service ignores SIGTERM and gets SIGKILL on stop timeout.
	var stubborn *fakeProc
	ops.startSvcFn = func(name string) (serviceProc, error) {
		stubborn = newFakeProc(200)
		stubborn.onKill = func() { stubborn.finish(errors.New("signal: killed")) }
		return stubborn, nil
	}
	ops.dialFn = func(network, addr string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	resp = roundTrip(t, srv, guestproto.Request{
		V:       guestproto.Version,
		Op:      guestproto.OpServiceStart,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd, Timeout: 1000},
	})
	if !resp.OK {
		t.Fatalf("start stubborn: %+v", resp)
	}
	resp = roundTrip(t, srv, guestproto.Request{
		V:       guestproto.Version,
		Op:      guestproto.OpServiceStop,
		Service: &guestproto.ServiceRequest{Name: guestproto.ServiceBuildkitd, Timeout: 25},
	})
	if !resp.OK || !stubborn.terminated.Load() || !stubborn.killed.Load() {
		t.Fatalf("want SIGTERM then SIGKILL, got resp=%+v term=%v kill=%v", resp, stubborn.terminated.Load(), stubborn.killed.Load())
	}
}
