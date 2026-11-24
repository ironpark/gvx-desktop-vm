package vmhelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop-vm/vz"
)

const (
	defaultStopTimeout        = 15 * time.Second
	defaultForceTimeout       = 5 * time.Second
	defaultAgentReadyWait     = 10 * time.Second
	defaultAgentPollEvery     = 5 * time.Millisecond
	defaultServiceTimeout     = 30 * time.Second
	defaultDiskTimeout        = 30 * time.Second
	defaultHelloTimeout       = 2 * time.Second
	defaultContainerStartWait = 30 * time.Second
	defaultContainerOpTimeout = 15 * time.Second
)

// vmInstance is the subset of *vz.VM that the helper server uses, extracted so
// unit tests can run against a fake VM and in-process guest agent.
type vmInstance interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Done() <-chan struct{}
	Err() error
	Connect(ctx context.Context, port uint32) (net.Conn, error)
	Close() error
}

type vmFactory func(cfg vz.Config) (vmInstance, error)

// serverOptions configures a helper Server. Zero fields use production defaults.
type serverOptions struct {
	dir           string
	helperVersion string
	newVM         vmFactory
	rosettaAvail  func() vz.Availability
	agentReady    time.Duration
	agentPoll     time.Duration
	serviceWait   time.Duration
	logf          func(format string, args ...any)
}

// Server serves vmproto over a control listener and manages one Linux VM.
//
// Lock order: lifecycleMu -> mu. Never block on I/O or channel writes while
// holding mu.
type Server struct {
	opts serverOptions

	srvCtx    context.Context
	srvCancel context.CancelFunc

	lifecycleMu sync.Mutex

	mu           sync.Mutex
	ln           net.Listener
	status       vmproto.Status
	vm           vmInstance
	spec         *vmproto.VMSpec
	startCh      chan struct{}
	startCancel  context.CancelFunc
	startErr     error
	startErrCode string
	stopCh       chan struct{}
	stopping     bool
	shuttingDown bool
	subscribers  map[chan vmproto.Event]struct{}
	activeDials  map[net.Conn]struct{}

	wg sync.WaitGroup
}

func newServer(opts serverOptions) *Server {
	if opts.newVM == nil {
		opts.newVM = func(cfg vz.Config) (vmInstance, error) {
			return vz.New(cfg)
		}
	}
	if opts.rosettaAvail == nil {
		opts.rosettaAvail = vz.RosettaAvailability
	}
	if opts.agentReady <= 0 {
		opts.agentReady = defaultAgentReadyWait
	}
	if opts.agentPoll <= 0 {
		opts.agentPoll = defaultAgentPollEvery
	}
	if opts.serviceWait <= 0 {
		opts.serviceWait = defaultServiceTimeout
	}
	if opts.helperVersion == "" {
		opts.helperVersion = "dev"
	}
	if opts.logf == nil {
		opts.logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "vmhelper: "+format+"\n", args...)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		opts:      opts,
		srvCtx:    ctx,
		srvCancel: cancel,
		status: vmproto.Status{
			State: vmproto.Stopped,
			Since: time.Now().UTC(),
		},
		subscribers: make(map[chan vmproto.Event]struct{}),
		activeDials: make(map[net.Conn]struct{}),
	}
}

// Serve accepts vmproto connections on ln until Shutdown is called or ln closes.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.wg.Wait()
			if s.isShuttingDown() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Shutdown stops the VM (gracefully within timeout, then by force) and closes
// the control listener so Serve returns.
func (s *Server) Shutdown(req vmproto.StopRequest) vmproto.Status {
	s.mu.Lock()
	s.shuttingDown = true
	ln := s.ln
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	st := s.stopVM(req)
	s.srvCancel()
	s.closeSubscribers()
	return st
}

func (s *Server) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shuttingDown
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	var req vmproto.Request
	if err := vmproto.ReadLine(br, &req); err != nil {
		if !errors.Is(err, io.EOF) {
			_ = vmproto.WriteLine(conn, vmproto.Response{
				OK:    false,
				Code:  vmproto.CodeBadRequest,
				Error: fmt.Sprintf("invalid request: %v", err),
			})
		}
		return
	}
	if req.V != vmproto.Version {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadVersion,
			Error: fmt.Sprintf("unsupported protocol version %d (want %d)", req.V, vmproto.Version),
		})
		return
	}

	switch req.Op {
	case vmproto.OpHello:
		s.handleHello(conn)
	case vmproto.OpStatus:
		s.handleStatus(conn)
	case vmproto.OpStart:
		s.handleStart(conn, req.Start)
	case vmproto.OpStop:
		s.handleStop(conn, req.Stop)
	case vmproto.OpDial:
		s.handleDial(conn, br, req.Dial)
	case vmproto.OpEvents:
		s.handleEvents(conn, br)
	case vmproto.OpShutdown:
		s.handleShutdown(conn, req.Stop)
	case vmproto.OpContainerStart:
		s.handleContainerStart(conn, req.Container)
	case vmproto.OpContainerAttach:
		s.handleContainerAttach(conn, br)
	case vmproto.OpContainerSignal:
		s.handleContainerSignal(conn, req.ContainerSignal)
	case vmproto.OpContainerResize:
		s.handleContainerResize(conn, req.ContainerResize)
	case vmproto.OpContainerExec:
		s.handleContainerExec(conn, br, req.ContainerExec)
	case vmproto.OpContainerTop:
		s.handleContainerTop(conn)
	case vmproto.OpContainerStats:
		s.handleContainerStats(conn)
	case vmproto.OpContainerPause:
		s.handleContainerPause(conn)
	case vmproto.OpContainerUnpause:
		s.handleContainerUnpause(conn)
	case vmproto.OpContainerStat:
		s.handleContainerStat(conn, req.ContainerCopy)
	case vmproto.OpContainerCopyFrom:
		s.handleContainerCopyFrom(conn, br, req.ContainerCopy)
	case vmproto.OpContainerCopyTo:
		s.handleContainerCopyTo(conn, br, req.ContainerCopy)
	case vmproto.OpContainerExport:
		s.handleContainerExport(conn, br, req.ContainerExport)
	case vmproto.OpContainerNetStream:
		s.handleContainerNetStream(conn, br, req.ContainerNet)
	case vmproto.OpContainerNetUpdate:
		s.handleContainerNetUpdate(conn, req.ContainerNet)
	default:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeUnknownOp,
			Error: fmt.Sprintf("unknown op %q", req.Op),
		})
	}
}

func (s *Server) handleHello(conn net.Conn) {
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK: true,
		Hello: &vmproto.Hello{
			Protocol:    vmproto.Version,
			MinProtocol: vmproto.Version,
			Helper:      s.opts.helperVersion,
			PID:         os.Getpid(),
			Features:    []string{vmproto.FeatureContainers, vmproto.FeatureOverlay},
		},
	})
}

func (s *Server) handleStatus(conn net.Conn) {
	st := s.snapshotStatus()
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:     true,
		Status: &st,
	})
}

func (s *Server) handleStart(conn net.Conn, req *vmproto.StartRequest) {
	if req == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "missing start request",
		})
		return
	}
	spec := req.Spec
	if err := validateSpec(spec); err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: err.Error(),
		})
		return
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeShutdown,
			Error: "helper is shutting down",
		})
		return
	}
	switch s.status.State {
	case vmproto.Running:
		same := s.spec != nil && specsEqual(*s.spec, spec)
		st := cloneStatus(s.status)
		s.mu.Unlock()
		if !same {
			_ = vmproto.WriteLine(conn, vmproto.Response{
				OK:     false,
				Code:   vmproto.CodeBusy,
				Error:  "VM is already running with a different spec",
				Status: &st,
			})
			return
		}
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true, Status: &st})
		return
	case vmproto.Stopping:
		st := cloneStatus(s.status)
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:     false,
			Code:   vmproto.CodeBusy,
			Error:  "VM is stopping",
			Status: &st,
		})
		return
	case vmproto.Starting:
		same := s.spec != nil && specsEqual(*s.spec, spec)
		ch := s.startCh
		st := cloneStatus(s.status)
		s.mu.Unlock()
		if !same {
			_ = vmproto.WriteLine(conn, vmproto.Response{
				OK:     false,
				Code:   vmproto.CodeBusy,
				Error:  "VM is starting with a different spec",
				Status: &st,
			})
			return
		}
		s.waitStartAndRespond(conn, ch)
		return
	}

	// Stopped or Failed: initiate single-flight start.
	startCtx, startCancel := context.WithCancel(s.srvCtx)
	ch := make(chan struct{})
	specCopy := cloneSpec(spec)
	s.spec = &specCopy
	s.startCh = ch
	s.startCancel = startCancel
	s.startErr = nil
	s.startErrCode = ""
	s.setStatusLocked(vmproto.Status{
		State: vmproto.Starting,
		Since: time.Now().UTC(),
		Spec:  &specCopy,
	})
	s.mu.Unlock()

	go s.runStart(startCtx, specCopy, ch)
	s.waitStartAndRespond(conn, ch)
}

func (s *Server) waitStartAndRespond(conn net.Conn, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-s.srvCtx.Done():
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeShutdown,
			Error: "helper is shutting down",
		})
		return
	}
	s.mu.Lock()
	err := s.startErr
	code := s.startErrCode
	st := cloneStatus(s.status)
	s.mu.Unlock()

	if err != nil {
		if code == "" {
			code = vmproto.CodeFailed
		}
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:     false,
			Code:   code,
			Error:  err.Error(),
			Status: &st,
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:     true,
		Status: &st,
	})
}

func (s *Server) runStart(ctx context.Context, spec vmproto.VMSpec, ch chan struct{}) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	vm, services, consolePath, code, err := s.bootVM(ctx, spec)

	s.mu.Lock()
	if s.startCancel != nil {
		s.startCancel()
		s.startCancel = nil
	}
	if err != nil {
		s.startErr = err
		s.startErrCode = code
		s.vm = nil
		s.spec = nil
		s.setStatusLocked(vmproto.Status{
			State:   vmproto.Failed,
			Err:     err.Error(),
			Since:   time.Now().UTC(),
			Console: consolePath,
		})
		close(ch)
		s.mu.Unlock()
		return
	}

	specCopy := cloneSpec(spec)
	s.vm = vm
	s.spec = &specCopy
	s.startErr = nil
	s.startErrCode = ""
	s.setStatusLocked(vmproto.Status{
		State:    vmproto.Running,
		Since:    time.Now().UTC(),
		Spec:     &specCopy,
		Services: services,
		Console:  consolePath,
	})
	close(ch)
	s.mu.Unlock()

	go s.watchVMExit(vm)
}

func (s *Server) bootVM(ctx context.Context, spec vmproto.VMSpec) (vmInstance, []vmproto.ServiceStatus, string, string, error) {
	if spec.Rosetta && s.opts.rosettaAvail() != vz.RosettaInstalled {
		return nil, nil, "", vmproto.CodeUnsupported, errors.New("Rosetta for Linux is not installed")
	}

	if spec.Disk != nil && spec.Disk.Path != "" {
		if err := ensureDisk(spec.Disk.Path, spec.Disk.Size); err != nil {
			return nil, nil, "", vmproto.CodeFailed, fmt.Errorf("prepare disk: %w", err)
		}
	}

	console, err := openConsoleSink(s.opts.dir)
	if err != nil {
		return nil, nil, "", vmproto.CodeFailed, fmt.Errorf("open console log: %w", err)
	}
	consolePath := console.Path()

	vzCfg := vz.Config{
		Kernel:      spec.Kernel,
		Initrd:      spec.Initrd,
		CommandLine: buildCommandLine(spec),
		CPUs:        spec.CPUs,
		MemoryBytes: uint64(spec.MemoryMiB) << 20,
		Console:     console.File(),
		Rosetta:     spec.Rosetta,
		RosettaTag:  guestproto.RosettaTag,
		NAT:         spec.Network,
		Balloon:     true,
	}
	if spec.Assets != "" {
		vzCfg.Shares = append(vzCfg.Shares, vz.Share{
			Tag:      guestproto.AssetsTag,
			Dir:      spec.Assets,
			ReadOnly: true,
		})
	}
	for _, sh := range spec.Shares {
		vzCfg.Shares = append(vzCfg.Shares, vz.Share{
			Tag:      sh.Tag,
			Dir:      sh.Dir,
			ReadOnly: sh.ReadOnly,
		})
	}
	if spec.Disk != nil && spec.Disk.Path != "" {
		vzCfg.Disks = append(vzCfg.Disks, vz.Disk{
			Path:     spec.Disk.Path,
			ReadOnly: false,
		})
	}
	for _, ed := range spec.ExtraDisks {
		vzCfg.Disks = append(vzCfg.Disks, vz.Disk{
			Path:     ed.Path,
			ReadOnly: ed.ReadOnly,
		})
	}

	vm, err := s.opts.newVM(vzCfg)
	// VZ duplicates the console write fd inside New; close our pipe write end
	// now so the console copier hits EOF when the VM exits.
	console.CloseWriter()
	if err != nil {
		code := vmproto.CodeFailed
		if errors.Is(err, vz.ErrUnsupported) {
			code = vmproto.CodeUnsupported
		}
		return nil, nil, consolePath, code, err
	}

	abortVM := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*defaultForceTimeout)
		defer cancel()
		_ = vm.Stop(stopCtx)
		_ = vm.Close()
	}

	if err := vm.Start(ctx); err != nil {
		abortVM()
		return nil, nil, consolePath, vmproto.CodeFailed, fmt.Errorf("start VM: %w", err)
	}

	if err := s.waitForAgent(ctx, vm); err != nil {
		abortVM()
		return nil, nil, consolePath, vmproto.CodeFailed, fmt.Errorf("wait for guest agent: %w", err)
	}

	if spec.Disk != nil && spec.Disk.Path != "" {
		diskCtx, cancel := context.WithTimeout(ctx, defaultDiskTimeout)
		_, err := agentDiskMount(diskCtx, vm, true)
		cancel()
		if err != nil {
			abortVM()
			return nil, nil, consolePath, errCodeFor(err), fmt.Errorf("mount guest disk: %w", err)
		}
	}

	var services []vmproto.ServiceStatus
	for _, svc := range spec.Services {
		svcCtx, cancel := context.WithTimeout(ctx, s.opts.serviceWait)
		got, err := agentServiceStart(svcCtx, vm, svc, s.opts.serviceWait)
		cancel()
		if err != nil {
			abortVM()
			return nil, nil, consolePath, errCodeFor(err), fmt.Errorf("start guest service %s: %w", svc, err)
		}
		services = toVMServices(got)
	}

	return vm, services, consolePath, "", nil
}

func (s *Server) waitForAgent(ctx context.Context, vm vmInstance) error {
	deadline := time.Now().Add(s.opts.agentReady)
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-vm.Done():
			if err := vm.Err(); err != nil {
				return fmt.Errorf("VM exited before agent answered: %w", err)
			}
			return errors.New("VM exited before agent answered")
		default:
		}

		tryCtx, cancel := context.WithTimeout(ctx, defaultHelloTimeout)
		_, err := agentHello(tryCtx, vm)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %w", s.opts.agentReady, lastErr)
		}
		timer := time.NewTimer(s.opts.agentPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-vm.Done():
			timer.Stop()
			if err := vm.Err(); err != nil {
				return fmt.Errorf("VM exited before agent answered: %w", err)
			}
			return errors.New("VM exited before agent answered")
		case <-timer.C:
		}
	}
}

func (s *Server) watchVMExit(vm vmInstance) {
	<-vm.Done()

	s.mu.Lock()
	if s.vm != vm || s.status.State != vmproto.Running {
		s.mu.Unlock()
		return
	}
	err := vm.Err()
	msg := "VM exited unexpectedly"
	if err != nil {
		msg = err.Error()
	}
	consolePath := s.status.Console
	s.vm = nil
	s.spec = nil
	conns := s.drainDialsLocked()
	s.setStatusLocked(vmproto.Status{
		State:   vmproto.Failed,
		Err:     msg,
		Since:   time.Now().UTC(),
		Console: consolePath,
	})
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	_ = vm.Close()
}

func (s *Server) handleStop(conn net.Conn, req *vmproto.StopRequest) {
	var sr vmproto.StopRequest
	if req != nil {
		sr = *req
	}
	st := s.stopVM(sr)
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:     true,
		Status: &st,
	})
}

func (s *Server) stopVM(req vmproto.StopRequest) vmproto.Status {
	// If a start is in flight, cancel it and wait for it to finish before stopping.
	s.mu.Lock()
	if s.status.State == vmproto.Starting {
		if s.startCancel != nil {
			s.startCancel()
		}
		ch := s.startCh
		s.mu.Unlock()
		if ch != nil {
			<-ch
		}
	} else if s.stopping {
		ch := s.stopCh
		s.mu.Unlock()
		if ch != nil {
			<-ch
		}
		return s.snapshotStatus()
	} else {
		s.mu.Unlock()
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.mu.Lock()
	if s.vm == nil {
		if s.status.State != vmproto.Stopped {
			s.setStatusLocked(vmproto.Status{
				State:   vmproto.Stopped,
				Since:   time.Now().UTC(),
				Console: s.status.Console,
			})
		}
		st := cloneStatus(s.status)
		s.mu.Unlock()
		return st
	}
	vm := s.vm
	var services []string
	if s.spec != nil {
		services = append([]string(nil), s.spec.Services...)
	}
	consolePath := s.status.Console
	stopCh := make(chan struct{})
	s.stopping = true
	s.stopCh = stopCh
	conns := s.drainDialsLocked()
	s.setStatusLocked(vmproto.Status{
		State:   vmproto.Stopping,
		Since:   time.Now().UTC(),
		Spec:    s.status.Spec,
		Console: consolePath,
	})
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}

	timeout := time.Duration(req.Timeout)
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}

	stoppedGracefully := false
	if !req.Force {
		graceCtx, cancel := context.WithTimeout(context.Background(), timeout)
		// Stop services in reverse order, then request guest poweroff.
		for i := len(services) - 1; i >= 0; i-- {
			if _, err := agentServiceStop(graceCtx, vm, services[i], timeout/2); err != nil {
				s.opts.logf("stop guest service %s: %v", services[i], err)
			}
		}
		if err := agentShutdown(graceCtx, vm); err != nil {
			s.opts.logf("guest shutdown op: %v", err)
		}
		select {
		case <-vm.Done():
			stoppedGracefully = true
		case <-graceCtx.Done():
		}
		cancel()
	}

	if !stoppedGracefully {
		forceCtx, cancel := context.WithTimeout(context.Background(), defaultForceTimeout)
		if err := vm.Stop(forceCtx); err != nil {
			s.opts.logf("force stop VM: %v", err)
		}
		cancel()
	}
	_ = vm.Close()

	s.mu.Lock()
	s.vm = nil
	s.spec = nil
	s.stopping = false
	s.stopCh = nil
	s.setStatusLocked(vmproto.Status{
		State:   vmproto.Stopped,
		Since:   time.Now().UTC(),
		Console: consolePath,
	})
	st := cloneStatus(s.status)
	close(stopCh)
	s.mu.Unlock()
	return st
}

func (s *Server) handleDial(conn net.Conn, br *bufio.Reader, req *vmproto.DialRequest) {
	if req == nil || (req.Service == "" && (req.Network == "" || req.Addr == "")) {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "dial requires service or network+addr",
		})
		return
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeShutdown,
			Error: "helper is shutting down",
		})
		return
	}
	vm := s.vm
	running := s.status.State == vmproto.Running
	s.mu.Unlock()

	if !running || vm == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeNotRunning,
			Error: "VM is not running",
		})
		return
	}

	dialCtx, cancel := context.WithTimeout(s.srvCtx, 10*time.Second)
	guestConn, err := agentDial(dialCtx, vm, guestproto.DialRequest{
		Service: req.Service,
		Network: req.Network,
		Addr:    req.Addr,
	})
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	defer guestConn.Close()

	if !s.trackDialConns(conn, guestConn) {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeNotRunning,
			Error: "VM stopped",
		})
		return
	}
	defer s.untrackDialConns(conn, guestConn)

	if err := vmproto.WriteLine(conn, vmproto.Response{OK: true}); err != nil {
		return
	}

	clientStream := newBufferedConn(conn, br)
	pipeBothWays(clientStream, conn, guestConn)
}

func (s *Server) runningVM(conn net.Conn) (vmInstance, bool) {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeShutdown,
			Error: "helper is shutting down",
		})
		return nil, false
	}
	vm := s.vm
	running := s.status.State == vmproto.Running
	s.mu.Unlock()

	if !running || vm == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeNotRunning,
			Error: "VM is not running",
		})
		return nil, false
	}
	return vm, true
}

func (s *Server) proxyContainerStream(conn net.Conn, br *bufio.Reader, guestConn net.Conn, okResp vmproto.Response) {
	defer guestConn.Close()
	if !s.trackDialConns(conn, guestConn) {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeNotRunning,
			Error: "VM stopped",
		})
		return
	}
	defer s.untrackDialConns(conn, guestConn)

	okResp.OK = true
	if err := vmproto.WriteLine(conn, okResp); err != nil {
		return
	}
	pipeBothWays(newBufferedConn(conn, br), conn, guestConn)
}

func (s *Server) handleContainerStart(conn net.Conn, req *vmproto.ContainerSpec) {
	if req == nil || len(req.Argv) == 0 {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-start requires container spec with argv",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerStartWait)
	st, err := agentContainerStart(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:        true,
		Container: &st,
	})
}

func (s *Server) handleContainerAttach(conn net.Conn, br *bufio.Reader) {
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	guestConn, st, err := agentContainerAttach(ctx, vm)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	s.proxyContainerStream(conn, br, guestConn, vmproto.Response{Container: st})
}

func (s *Server) handleContainerSignal(conn net.Conn, req *vmproto.ContainerSignalRequest) {
	if req == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-signal requires signal request",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	err := agentContainerSignal(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
}

func (s *Server) handleContainerResize(conn net.Conn, req *vmproto.ContainerResizeRequest) {
	if req == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-resize requires rows and cols",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	err := agentContainerResize(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
}

func (s *Server) handleContainerExec(conn net.Conn, br *bufio.Reader, req *vmproto.ContainerExecRequest) {
	if req == nil || len(req.Argv) == 0 {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-exec requires argv",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	guestConn, pid, err := agentContainerExec(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	s.proxyContainerStream(conn, br, guestConn, vmproto.Response{ExecPID: pid})
}

func (s *Server) handleContainerTop(conn net.Conn) {
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	procs, err := agentContainerTop(ctx, vm)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:        true,
		Processes: procs,
	})
}

func (s *Server) handleContainerStats(conn net.Conn) {
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	stats, err := agentContainerStats(ctx, vm)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:    true,
		Stats: &stats,
	})
}

func (s *Server) handleContainerPause(conn net.Conn) {
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	err := agentContainerPause(ctx, vm)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
}

func (s *Server) handleContainerUnpause(conn net.Conn) {
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	err := agentContainerUnpause(ctx, vm)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
}

func (s *Server) handleContainerStat(conn net.Conn, req *vmproto.ContainerCopyRequest) {
	if req == nil || req.Path == "" {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-stat requires path",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	ps, err := agentContainerStat(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:       true,
		PathStat: &ps,
	})
}

func (s *Server) handleContainerCopyFrom(conn net.Conn, br *bufio.Reader, req *vmproto.ContainerCopyRequest) {
	if req == nil || req.Path == "" {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-copy-from requires path",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	guestConn, ps, err := agentContainerCopyFrom(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	s.proxyContainerStream(conn, br, guestConn, vmproto.Response{PathStat: ps})
}

func (s *Server) handleContainerCopyTo(conn net.Conn, br *bufio.Reader, req *vmproto.ContainerCopyRequest) {
	if req == nil || req.Path == "" {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-copy-to requires path",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	guestConn, err := agentContainerCopyTo(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	s.proxyContainerStream(conn, br, guestConn, vmproto.Response{})
}

func (s *Server) handleContainerExport(conn net.Conn, br *bufio.Reader, req *vmproto.ContainerExportRequest) {
	var er vmproto.ContainerExportRequest
	if req != nil {
		er = *req
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	guestConn, err := agentContainerExport(ctx, vm, er)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	s.proxyContainerStream(conn, br, guestConn, vmproto.Response{})
}

func (s *Server) handleContainerNetStream(conn net.Conn, br *bufio.Reader, req *vmproto.ContainerNetConfig) {
	if req == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-net-stream requires containerNet config",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	guestConn, err := agentContainerNetStream(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	s.proxyContainerStream(conn, br, guestConn, vmproto.Response{})
}

func (s *Server) handleContainerNetUpdate(conn net.Conn, req *vmproto.ContainerNetConfig) {
	if req == nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeBadRequest,
			Error: "container-net-update requires containerNet config",
		})
		return
	}
	vm, ok := s.runningVM(conn)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.srvCtx, defaultContainerOpTimeout)
	err := agentContainerNetUpdate(ctx, vm, *req)
	cancel()
	if err != nil {
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  errCodeFor(err),
			Error: err.Error(),
		})
		return
	}
	_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
}

func (s *Server) handleEvents(conn net.Conn, br *bufio.Reader) {
	ch := make(chan vmproto.Event, 16)
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    false,
			Code:  vmproto.CodeShutdown,
			Error: "helper is shutting down",
		})
		return
	}
	s.subscribers[ch] = struct{}{}
	initial := vmproto.Event{
		Time:   time.Now().UTC(),
		Status: cloneStatus(s.status),
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
	}()

	if err := vmproto.WriteLine(conn, vmproto.Response{OK: true}); err != nil {
		return
	}
	if err := vmproto.WriteLine(conn, initial); err != nil {
		return
	}

	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		_, _ = io.Copy(io.Discard, br)
	}()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := vmproto.WriteLine(conn, ev); err != nil {
				return
			}
		case <-clientGone:
			return
		case <-s.srvCtx.Done():
			return
		}
	}
}

func (s *Server) handleShutdown(conn net.Conn, req *vmproto.StopRequest) {
	var sr vmproto.StopRequest
	if req != nil {
		sr = *req
	}
	s.mu.Lock()
	s.shuttingDown = true
	ln := s.ln
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	st := s.stopVM(sr)
	_ = vmproto.WriteLine(conn, vmproto.Response{
		OK:     true,
		Status: &st,
	})
	s.srvCancel()
	s.closeSubscribers()
}

func (s *Server) snapshotStatus() vmproto.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStatus(s.status)
}

// setStatusLocked updates s.status and broadcasts to event subscribers without
// blocking. Caller holds s.mu.
func (s *Server) setStatusLocked(st vmproto.Status) {
	s.status = st
	ev := vmproto.Event{
		Time:   time.Now().UTC(),
		Status: cloneStatus(st),
	}
	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (s *Server) closeSubscribers() {
	s.mu.Lock()
	for ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, ch)
	}
	s.mu.Unlock()
}

func (s *Server) trackDialConns(conns ...net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown || s.status.State != vmproto.Running {
		return false
	}
	for _, c := range conns {
		s.activeDials[c] = struct{}{}
	}
	return true
}

func (s *Server) untrackDialConns(conns ...net.Conn) {
	s.mu.Lock()
	for _, c := range conns {
		delete(s.activeDials, c)
	}
	s.mu.Unlock()
}

func (s *Server) drainDialsLocked() []net.Conn {
	conns := make([]net.Conn, 0, len(s.activeDials))
	for c := range s.activeDials {
		conns = append(conns, c)
	}
	clear(s.activeDials)
	return conns
}

// pipeBothWays forwards bytes bidirectionally between the control-socket client
// and the guest stream. Half-close from the client (CloseWrite) is preserved so
// any response bytes from the guest still flush back even though VZ vsock has
// no half-close; a full client Close wakes immediately via waitPeerClose.
func pipeBothWays(client, rawClient, guest net.Conn) {
	closeWrite := func(c net.Conn) {
		if cw, ok := c.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}

	guestDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(guest, client)
		if err != nil {
			_ = guest.Close()
			return
		}
		if cw, ok := guest.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		if waitPeerClose(rawClient, guestDone) {
			_ = guest.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, guest)
		closeWrite(client)
		close(guestDone)
		_ = client.Close()
		_ = guest.Close()
	}()
	wg.Wait()
}

func validateSpec(spec vmproto.VMSpec) error {
	if strings.TrimSpace(spec.Kernel) == "" {
		return errors.New("spec.kernel is required")
	}
	if spec.CPUs <= 0 {
		return errors.New("spec.cpus must be positive")
	}
	if spec.MemoryMiB <= 0 {
		return errors.New("spec.memoryMiB must be positive")
	}
	seenTags := make(map[string]struct{}, len(spec.Shares))
	for _, sh := range spec.Shares {
		if !isValidShareTag(sh.Tag) {
			return fmt.Errorf("invalid share tag %q", sh.Tag)
		}
		if sh.Tag == guestproto.AssetsTag || sh.Tag == guestproto.RosettaTag {
			return fmt.Errorf("share tag %q is reserved", sh.Tag)
		}
		if _, dup := seenTags[sh.Tag]; dup {
			return fmt.Errorf("duplicate share tag %q", sh.Tag)
		}
		seenTags[sh.Tag] = struct{}{}
		if strings.TrimSpace(sh.Dir) == "" {
			return fmt.Errorf("share %q requires dir", sh.Tag)
		}
	}
	for i, ed := range spec.ExtraDisks {
		if strings.TrimSpace(ed.Path) == "" {
			return fmt.Errorf("extraDisks[%d] requires path", i)
		}
	}
	return nil
}

// isValidShareTag restricts virtiofs share tags to safe path and shell
// characters ([A-Za-z0-9._-]), rejecting ".", "..", and leading "-".
func isValidShareTag(tag string) bool {
	if tag == "" || tag == "." || tag == ".." || tag[0] == '-' {
		return false
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func ensureDisk(path string, size int64) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
	}
	return f.Close()
}

func buildCommandLine(spec vmproto.VMSpec) string {
	parts := []string{"console=hvc0", "panic=-1", "quiet"}
	if spec.Assets != "" {
		parts = append(parts, guestproto.CmdlineAssets+"="+guestproto.AssetsTag)
	}
	if spec.Rosetta {
		parts = append(parts, guestproto.CmdlineRosetta+"="+guestproto.RosettaTag)
	}
	if spec.Network {
		parts = append(parts, guestproto.CmdlineNet+"=1")
	}
	if len(spec.Shares) > 0 {
		tags := make([]string, 0, len(spec.Shares))
		for _, sh := range spec.Shares {
			if sh.ReadOnly {
				tags = append(tags, sh.Tag+":ro")
			} else {
				tags = append(tags, sh.Tag)
			}
		}
		parts = append(parts, guestproto.CmdlineShares+"="+strings.Join(tags, ","))
	}
	if extra := strings.TrimSpace(spec.CommandLine); extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, " ")
}

func errCodeFor(err error) string {
	var ae *agentError
	if errors.As(err, &ae) {
		return ae.vmProtoCode()
	}
	if errors.Is(err, vz.ErrUnsupported) {
		return vmproto.CodeUnsupported
	}
	return vmproto.CodeFailed
}

func toVMServices(in []guestproto.ServiceStatus) []vmproto.ServiceStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]vmproto.ServiceStatus, len(in))
	for i, s := range in {
		out[i] = vmproto.ServiceStatus{
			Name:  s.Name,
			State: s.State,
			Err:   s.Err,
		}
	}
	return out
}

func cloneSpec(s vmproto.VMSpec) vmproto.VMSpec {
	cp := s
	if s.Disk != nil {
		d := *s.Disk
		cp.Disk = &d
	}
	if len(s.Services) > 0 {
		cp.Services = append([]string(nil), s.Services...)
	}
	if len(s.Shares) > 0 {
		cp.Shares = append([]vmproto.ShareSpec(nil), s.Shares...)
	}
	if len(s.ExtraDisks) > 0 {
		cp.ExtraDisks = append([]vmproto.ExtraDiskSpec(nil), s.ExtraDisks...)
	}
	return cp
}

func cloneStatus(st vmproto.Status) vmproto.Status {
	cp := st
	if st.Spec != nil {
		sc := cloneSpec(*st.Spec)
		cp.Spec = &sc
	}
	if len(st.Services) > 0 {
		cp.Services = append([]vmproto.ServiceStatus(nil), st.Services...)
	}
	return cp
}

func specsEqual(a, b vmproto.VMSpec) bool {
	if a.Kernel != b.Kernel ||
		a.Initrd != b.Initrd ||
		a.Assets != b.Assets ||
		a.CPUs != b.CPUs ||
		a.MemoryMiB != b.MemoryMiB ||
		a.Rosetta != b.Rosetta ||
		a.Network != b.Network ||
		a.CommandLine != b.CommandLine {
		return false
	}
	if (a.Disk == nil) != (b.Disk == nil) {
		return false
	}
	if a.Disk != nil && (a.Disk.Path != b.Disk.Path || a.Disk.Size != b.Disk.Size) {
		return false
	}
	return slices.Equal(a.Services, b.Services) &&
		slices.Equal(a.Shares, b.Shares) &&
		slices.Equal(a.ExtraDisks, b.ExtraDisks)
}
