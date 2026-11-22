package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

// vsockListener accepts raw stream connections (virtio-vsock on Linux).
type vsockListener interface {
	Accept() (io.ReadWriteCloser, error)
	Close() error
}

// hostInfo holds the guest OS details reported by OpHello.
type hostInfo struct {
	Kernel  string
	Machine string
	Rosetta bool
	Uptime  float64
}

// serviceSpec describes a named guest service the agent can manage.
type serviceSpec struct {
	Name    string
	Network string
	Addr    string
}

// serviceProc is a running service process managed by the agent.
type serviceProc interface {
	PID() int
	Done() <-chan error
	Terminate() error
	Kill() error
	LogTail() string
}

// sysOps abstracts the Linux-specific operations so the protocol server
// and its unit tests stay portable across darwin, linux and windows.
type sysOps interface {
	HostInfo() hostInfo
	MountDisk(format bool) (guestproto.DiskStatus, error)
	UnmountDisk() error
	Sync()
	PowerOff()
	StartService(name string) (serviceProc, error)
	Dial(network, addr string) (net.Conn, error)
}

var knownServices = map[string]serviceSpec{
	guestproto.ServiceBuildkitd: {
		Name:    guestproto.ServiceBuildkitd,
		Network: "unix",
		Addr:    guestproto.BuildkitdSocket,
	},
}

var serviceOrder = []string{guestproto.ServiceBuildkitd}

const (
	defaultServiceStartTimeout = 30 * time.Second
	defaultServiceStopTimeout  = 10 * time.Second
	defaultShutdownStopTimeout = 5 * time.Second
	servicePollInterval        = 50 * time.Millisecond

	// vsockHostCID equals Linux's VMADDR_CID_HOST (2), the only peer CID
	// allowed to talk to the guest agent.
	vsockHostCID uint32 = 2
)

// isAllowedVsockCID reports whether a connecting vsock peer CID is the host.
// Connections from guest-local processes (VMADDR_CID_LOCAL=1, the guest's
// own CID, etc.) are rejected so build containers cannot invoke privileged
// agent ops such as OpExec or OpShutdown.
func isAllowedVsockCID(cid uint32) bool {
	return cid == vsockHostCID
}

type serviceEntry struct {
	mu           sync.Mutex
	spec         serviceSpec
	state        string
	pid          int
	errMsg       string
	proc         serviceProc
	stopping     bool
	pollInterval time.Duration
}

// Server serves guestproto requests over connections from the VM helper.
type Server struct {
	version       string
	ops           sysOps
	diskMu        sync.Mutex
	services      map[string]*serviceEntry
	shutdownOnce  sync.Once
	shutdownDelay time.Duration
}

// NewServer constructs a guestproto server.
func NewServer(version string, ops sysOps) *Server {
	svcs := make(map[string]*serviceEntry, len(knownServices))
	for name, spec := range knownServices {
		svcs[name] = &serviceEntry{
			spec:         spec,
			state:        "stopped",
			pollInterval: servicePollInterval,
		}
	}
	return &Server{
		version:       version,
		ops:           ops,
		services:      svcs,
		shutdownDelay: 50 * time.Millisecond,
	}
}

// ServeConn handles one guestproto connection and closes it on return.
func (s *Server) ServeConn(conn io.ReadWriteCloser) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	var req guestproto.Request
	if err := vmproto.ReadLine(br, &req); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return
		}
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeBadRequest,
			Error: err.Error(),
		})
		return
	}

	if req.V != guestproto.Version {
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeBadVersion,
			Error: fmt.Sprintf("unsupported protocol version %d (want %d)", req.V, guestproto.Version),
		})
		return
	}

	switch req.Op {
	case guestproto.OpHello:
		s.handleHello(conn)
	case guestproto.OpDiskMount:
		s.handleDiskMount(conn, req.Disk)
	case guestproto.OpServiceStart:
		s.handleServiceStart(conn, req.Service)
	case guestproto.OpServiceStop:
		s.handleServiceStop(conn, req.Service)
	case guestproto.OpServiceStatus:
		s.handleServiceStatus(conn)
	case guestproto.OpExec:
		s.handleExec(conn, br, req.Exec)
	case guestproto.OpDial:
		s.handleDial(conn, br, req.Dial)
	case guestproto.OpShutdown:
		s.handleShutdown(conn)
	default:
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeUnknownOp,
			Error: "unknown op " + req.Op,
		})
	}
}

func (s *Server) handleHello(w io.Writer) {
	info := s.ops.HostInfo()
	_ = vmproto.WriteLine(w, guestproto.Response{
		OK: true,
		Hello: &guestproto.Hello{
			Protocol:    guestproto.Version,
			MinProtocol: guestproto.Version,
			Agent:       s.version,
			Kernel:      info.Kernel,
			Machine:     info.Machine,
			Rosetta:     info.Rosetta,
			Uptime:      info.Uptime,
		},
	})
}

func (s *Server) handleDiskMount(w io.Writer, req *guestproto.DiskRequest) {
	format := false
	if req != nil {
		format = req.Format
	}
	s.diskMu.Lock()
	status, err := s.ops.MountDisk(format)
	s.diskMu.Unlock()
	if err != nil {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeFailed,
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(w, guestproto.Response{
		OK:   true,
		Disk: &status,
	})
}

func (s *Server) handleServiceStatus(w io.Writer) {
	_ = vmproto.WriteLine(w, guestproto.Response{
		OK:       true,
		Services: s.allServiceStatus(),
	})
}

func (s *Server) allServiceStatus() []guestproto.ServiceStatus {
	out := make([]guestproto.ServiceStatus, 0, len(serviceOrder))
	for _, name := range serviceOrder {
		e := s.services[name]
		e.mu.Lock()
		out = append(out, guestproto.ServiceStatus{
			Name:  e.spec.Name,
			State: e.state,
			PID:   e.pid,
			Err:   e.errMsg,
		})
		e.mu.Unlock()
	}
	return out
}

func (s *Server) handleServiceStart(w io.Writer, req *guestproto.ServiceRequest) {
	if req == nil || req.Name == "" {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeBadRequest,
			Error: "service name is required",
		})
		return
	}
	e, ok := s.services[req.Name]
	if !ok {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeNoService,
			Error: "unknown service " + req.Name,
		})
		return
	}

	timeout := defaultServiceStartTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Millisecond
	}

	if err := s.startService(e, timeout); err != nil {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:       false,
			Code:     guestproto.CodeFailed,
			Error:    err.Error(),
			Services: s.allServiceStatus(),
		})
		return
	}
	_ = vmproto.WriteLine(w, guestproto.Response{
		OK:       true,
		Services: s.allServiceStatus(),
	})
}

func (s *Server) startService(e *serviceEntry, timeout time.Duration) error {
	e.mu.Lock()
	if e.state == "running" && e.proc != nil {
		e.mu.Unlock()
		return nil
	}
	e.state = "starting"
	e.pid = 0
	e.errMsg = ""
	e.stopping = false

	proc, err := s.ops.StartService(e.spec.Name)
	if err != nil {
		e.state = "failed"
		e.errMsg = err.Error()
		e.mu.Unlock()
		return err
	}
	e.proc = proc
	e.pid = proc.PID()
	poll := e.pollInterval
	if poll <= 0 {
		poll = servicePollInterval
	}
	e.mu.Unlock()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case exitErr := <-proc.Done():
			msg := formatServiceExit(e.spec.Name, exitErr, proc.LogTail())
			e.mu.Lock()
			if e.proc == proc {
				e.proc = nil
				e.pid = 0
				e.state = "failed"
				e.errMsg = msg
			}
			e.mu.Unlock()
			return errors.New(msg)
		default:
		}

		c, dialErr := s.ops.Dial(e.spec.Network, e.spec.Addr)
		if dialErr == nil {
			_ = c.Close()
			e.mu.Lock()
			if e.proc == proc {
				e.state = "running"
				e.errMsg = ""
			}
			e.mu.Unlock()
			go s.watchService(e, proc)
			return nil
		}

		if time.Now().After(deadline) {
			_ = proc.Kill()
			<-proc.Done()
			msg := fmt.Sprintf("service %s did not become ready on %s within %s", e.spec.Name, e.spec.Addr, timeout)
			if tail := proc.LogTail(); tail != "" {
				msg += ": " + tail
			}
			e.mu.Lock()
			if e.proc == proc {
				e.proc = nil
				e.pid = 0
				e.state = "failed"
				e.errMsg = msg
			}
			e.mu.Unlock()
			return errors.New(msg)
		}

		select {
		case exitErr := <-proc.Done():
			msg := formatServiceExit(e.spec.Name, exitErr, proc.LogTail())
			e.mu.Lock()
			if e.proc == proc {
				e.proc = nil
				e.pid = 0
				e.state = "failed"
				e.errMsg = msg
			}
			e.mu.Unlock()
			return errors.New(msg)
		case <-ticker.C:
		}
	}
}

func (s *Server) watchService(e *serviceEntry, proc serviceProc) {
	exitErr := <-proc.Done()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.proc != proc {
		return
	}
	e.proc = nil
	e.pid = 0
	if e.stopping {
		e.state = "stopped"
		e.errMsg = ""
		e.stopping = false
		return
	}
	e.state = "failed"
	e.errMsg = formatServiceExit(e.spec.Name, exitErr, proc.LogTail())
	log.Printf("%s", e.errMsg)
}

func formatServiceExit(name string, err error, logTail string) string {
	msg := fmt.Sprintf("service %s exited unexpectedly", name)
	if err != nil {
		msg = fmt.Sprintf("service %s exited: %v", name, err)
	}
	if logTail != "" {
		msg += " (" + logTail + ")"
	}
	return msg
}

func (s *Server) handleServiceStop(w io.Writer, req *guestproto.ServiceRequest) {
	if req == nil || req.Name == "" {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeBadRequest,
			Error: "service name is required",
		})
		return
	}
	e, ok := s.services[req.Name]
	if !ok {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeNoService,
			Error: "unknown service " + req.Name,
		})
		return
	}

	timeout := defaultServiceStopTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Millisecond
	}

	if err := s.stopService(e, timeout); err != nil {
		_ = vmproto.WriteLine(w, guestproto.Response{
			OK:       false,
			Code:     guestproto.CodeFailed,
			Error:    err.Error(),
			Services: s.allServiceStatus(),
		})
		return
	}
	_ = vmproto.WriteLine(w, guestproto.Response{
		OK:       true,
		Services: s.allServiceStatus(),
	})
}

func (s *Server) stopService(e *serviceEntry, timeout time.Duration) error {
	e.mu.Lock()
	proc := e.proc
	if proc == nil {
		e.state = "stopped"
		e.pid = 0
		e.stopping = false
		e.mu.Unlock()
		return nil
	}
	e.stopping = true
	e.mu.Unlock()

	_ = proc.Terminate()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-proc.Done():
	case <-timer.C:
		_ = proc.Kill()
		<-proc.Done()
	}

	e.mu.Lock()
	if e.proc == proc {
		e.proc = nil
		e.pid = 0
		e.state = "stopped"
		e.errMsg = ""
		e.stopping = false
	}
	e.mu.Unlock()
	return nil
}

// frameWriter turns a command's stdout/stderr writes into framed guestproto
// messages, chunking writes larger than guestproto.MaxFrame.
type frameWriter struct {
	mu   *sync.Mutex
	w    io.Writer
	kind byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > guestproto.MaxFrame {
			n = guestproto.MaxFrame
		}
		f.mu.Lock()
		err := guestproto.WriteFrame(f.w, f.kind, p[:n])
		f.mu.Unlock()
		if err != nil {
			return total - len(p), err
		}
		p = p[n:]
	}
	return total, nil
}

// handleExec executes a command requested by the host VM helper.
//
// Access is restricted to the host hypervisor by the vsock listener's
// VMADDR_CID_HOST peer check (ops_linux.go); guest processes and buildkit
// RUN containers cannot connect to the agent. OpExec is retained solely for
// host-side integration tests, diagnostics, and helper-driven maintenance
// operations rather than routine build forwarding (which uses OpDial).
func (s *Server) handleExec(conn io.ReadWriteCloser, br *bufio.Reader, req *guestproto.ExecRequest) {
	if req == nil || len(req.Argv) == 0 {
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeBadRequest,
			Error: "exec requires argv",
		})
		return
	}
	if err := vmproto.WriteLine(conn, guestproto.Response{OK: true}); err != nil {
		return
	}

	var mu sync.Mutex
	writeExit := func(code int, elapsed time.Duration, errMsg string) {
		payload, _ := json.Marshal(guestproto.Exit{
			Code:    code,
			Elapsed: elapsed.Nanoseconds(),
			Error:   errMsg,
		})
		mu.Lock()
		defer mu.Unlock()
		_ = guestproto.WriteFrame(conn, guestproto.FrameExit, payload)
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Dir = req.Dir
	cmd.Stdout = frameWriter{mu: &mu, w: conn, kind: guestproto.FrameStdout}
	cmd.Stderr = frameWriter{mu: &mu, w: conn, kind: guestproto.FrameStderr}

	var stdinPipe io.WriteCloser
	if req.Stdin {
		var err error
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			writeExit(127, 0, err.Error())
			return
		}
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		writeExit(127, time.Since(start), err.Error())
		return
	}

	if stdinPipe != nil {
		go func() {
			defer stdinPipe.Close()
			for {
				kind, payload, err := guestproto.ReadFrame(br)
				if err != nil {
					return
				}
				switch kind {
				case guestproto.FrameStdin:
					if len(payload) > 0 {
						if _, err := stdinPipe.Write(payload); err != nil {
							return
						}
					}
				case guestproto.FrameStdinEOF:
					return
				}
			}
		}()
	}

	err := cmd.Wait()
	elapsed := time.Since(start)
	var ee *exec.ExitError
	switch {
	case err == nil:
		writeExit(0, elapsed, "")
	case errors.As(err, &ee):
		writeExit(ee.ExitCode(), elapsed, "")
	default:
		writeExit(127, elapsed, err.Error())
	}
}

func (s *Server) handleDial(conn io.ReadWriteCloser, br *bufio.Reader, req *guestproto.DialRequest) {
	if req == nil {
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeBadRequest,
			Error: "dial request is required",
		})
		return
	}

	var network, addr string
	if req.Service != "" {
		spec, ok := knownServices[req.Service]
		if !ok {
			_ = vmproto.WriteLine(conn, guestproto.Response{
				OK:    false,
				Code:  guestproto.CodeNoService,
				Error: "unknown service " + req.Service,
			})
			return
		}
		network, addr = spec.Network, spec.Addr
	} else {
		if (req.Network != "unix" && req.Network != "tcp") || req.Addr == "" {
			_ = vmproto.WriteLine(conn, guestproto.Response{
				OK:    false,
				Code:  guestproto.CodeBadRequest,
				Error: "dial requires service or network (unix|tcp) and addr",
			})
			return
		}
		network, addr = req.Network, req.Addr
	}

	target, err := s.ops.Dial(network, addr)
	if err != nil {
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:    false,
			Code:  guestproto.CodeFailed,
			Error: err.Error(),
		})
		return
	}
	defer target.Close()

	if err := vmproto.WriteLine(conn, guestproto.Response{OK: true}); err != nil {
		return
	}

	// VZ vsock has no half-close: when either side closes, close both so
	// neither copy goroutine hangs.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(target, br)
		_ = target.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, target)
		_ = conn.Close()
	}()
	wg.Wait()
}

func (s *Server) handleShutdown(conn io.ReadWriteCloser) {
	// Stop all services, sync, and unmount the persistent disk before
	// replying so the filesystem is clean before poweroff.
	for _, name := range serviceOrder {
		if e, ok := s.services[name]; ok {
			_ = s.stopService(e, defaultShutdownStopTimeout)
		}
	}
	s.ops.Sync()
	s.diskMu.Lock()
	if err := s.ops.UnmountDisk(); err != nil {
		log.Printf("unmount disk: %v", err)
	}
	s.diskMu.Unlock()
	s.ops.Sync()

	_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
	_ = conn.Close()

	s.shutdownOnce.Do(func() {
		delay := s.shutdownDelay
		go func() {
			if delay > 0 {
				time.Sleep(delay)
			}
			s.ops.Sync()
			s.ops.PowerOff()
		}()
	})
}

// tailBuffer keeps the last max bytes written, for error reporting.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.max {
		t.buf.Reset()
		t.buf.Write(p[len(p)-t.max:])
		return len(p), nil
	}
	if excess := t.buf.Len() + len(p) - t.max; excess > 0 {
		t.buf.Next(excess)
	}
	t.buf.Write(p)
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.buf.Bytes()))
}
