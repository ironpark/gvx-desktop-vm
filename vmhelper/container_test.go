package vmhelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop-vm/vz"
)

// fakeContainerVM extends fakeVM behavior with full container-* guestproto ops.
type fakeContainerVM struct {
	*fakeVM

	ctrMu          sync.Mutex
	startedSpec    *guestproto.ContainerSpec
	paused         bool
	signals        []guestproto.ContainerSignalRequest
	resizes        []guestproto.ContainerResizeRequest
	execReqs       []guestproto.ContainerExecRequest
	execResizes    []guestproto.ContainerResizeRequest
	copiedToPath   string
	copiedToData   []byte
	copyToFailMsg  string
	copyFromErrMsg string
	exportReqs     []guestproto.ContainerExportRequest
}

func newFakeContainerVM(cfg vz.Config) *fakeContainerVM {
	return &fakeContainerVM{fakeVM: newFakeVM(cfg)}
}

func (f *fakeContainerVM) Connect(ctx context.Context, port uint32) (net.Conn, error) {
	if port != guestproto.AgentPort {
		return nil, errors.New("connection refused")
	}
	select {
	case <-f.done:
		return nil, errors.New("vm stopped")
	default:
	}
	client, server := net.Pipe()
	go f.serveContainerAgent(server)
	return client, nil
}

func (f *fakeContainerVM) serveContainerAgent(conn net.Conn) {
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
				Agent:       "fake-2",
				Kernel:      "6.18.64-0-virt",
				Machine:     "aarch64",
				Rosetta:     f.cfg.Rosetta,
				Uptime:      0.2,
				Features:    []string{guestproto.FeatureContainers},
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
	case guestproto.OpShutdown:
		f.shutdownRecv.Store(true)
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
		go func() {
			time.Sleep(10 * time.Millisecond)
			f.finish(nil)
		}()
	case guestproto.OpContainerStart:
		if req.Container == nil || len(req.Container.Argv) == 0 {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeBadRequest, Error: "missing argv"})
			return
		}
		cp := *req.Container
		f.ctrMu.Lock()
		f.startedSpec = &cp
		f.paused = false
		f.ctrMu.Unlock()
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK:        true,
			Container: &guestproto.ContainerState{Running: true, PID: 1},
		})
	case guestproto.OpContainerAttach:
		f.ctrMu.Lock()
		started := f.startedSpec != nil
		f.ctrMu.Unlock()
		if !started {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeNotRunning, Error: "container not started"})
			return
		}
		if err := vmproto.WriteLine(conn, guestproto.Response{
			OK:        true,
			Container: &guestproto.ContainerState{Running: true, PID: 1},
		}); err != nil {
			return
		}
		// Send initial buffered output, echo stdin until EOF, record resize, then exit.
		_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, []byte("booted\n"))
		_ = guestproto.WriteFrame(conn, guestproto.FrameStderr, []byte("warn\n"))
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				return
			}
			switch kind {
			case guestproto.FrameStdin:
				_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, append([]byte("in:"), payload...))
			case guestproto.FrameResize:
				var rz guestproto.ContainerResizeRequest
				if json.Unmarshal(payload, &rz) == nil {
					f.ctrMu.Lock()
					f.resizes = append(f.resizes, rz)
					f.ctrMu.Unlock()
				}
			case guestproto.FrameStdinEOF:
				ex, _ := json.Marshal(guestproto.Exit{Code: 0, Elapsed: 1234})
				_ = guestproto.WriteFrame(conn, guestproto.FrameExit, ex)
				return
			}
		}
	case guestproto.OpContainerSignal:
		f.ctrMu.Lock()
		f.signals = append(f.signals, *req.ContainerSignal)
		f.ctrMu.Unlock()
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
	case guestproto.OpContainerResize:
		f.ctrMu.Lock()
		f.resizes = append(f.resizes, *req.ContainerResize)
		f.ctrMu.Unlock()
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
	case guestproto.OpContainerExec:
		f.ctrMu.Lock()
		paused := f.paused
		if !paused && req.ContainerExec != nil {
			f.execReqs = append(f.execReqs, *req.ContainerExec)
		}
		f.ctrMu.Unlock()
		if paused {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodePaused, Error: "container is paused"})
			return
		}
		if err := vmproto.WriteLine(conn, guestproto.Response{OK: true, ExecPID: 77}); err != nil {
			return
		}
		// Special argv "hold" blocks until client disconnects (tests detach/stop).
		if len(req.ContainerExec.Argv) > 0 && req.ContainerExec.Argv[0] == "hold" {
			_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, []byte("holding"))
			_, _, _ = guestproto.ReadFrame(br)
			return
		}
		var stdinBuf bytes.Buffer
		if req.ContainerExec.Interactive {
			for {
				kind, payload, err := guestproto.ReadFrame(br)
				if err != nil {
					return
				}
				switch kind {
				case guestproto.FrameStdin:
					stdinBuf.Write(payload)
				case guestproto.FrameResize:
					var rz guestproto.ContainerResizeRequest
					if json.Unmarshal(payload, &rz) == nil {
						f.ctrMu.Lock()
						f.execResizes = append(f.execResizes, rz)
						f.ctrMu.Unlock()
					}
				case guestproto.FrameStdinEOF:
					goto doneReading
				}
			}
		}
	doneReading:
		out := strings.Join(req.ContainerExec.Argv, " ")
		if stdinBuf.Len() > 0 {
			out += "|" + stdinBuf.String()
		}
		_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, []byte(out))
		_ = guestproto.WriteFrame(conn, guestproto.FrameStderr, []byte("exec-err"))
		code := 0
		if len(req.ContainerExec.Argv) > 0 && req.ContainerExec.Argv[0] == "fail42" {
			code = 42
		}
		ex, _ := json.Marshal(guestproto.Exit{Code: code, Elapsed: 55})
		_ = guestproto.WriteFrame(conn, guestproto.FrameExit, ex)
	case guestproto.OpContainerTop:
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK: true,
			Processes: []guestproto.ContainerProcess{
				{PID: 1, PPID: 0, UID: 0, GID: 0, State: "S", Threads: 1, Comm: "sh", Cmdline: []string{"/bin/sh"}},
			},
		})
	case guestproto.OpContainerStats:
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK: true,
			Stats: &guestproto.ContainerStats{
				Timestamp:        time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
				CPUUserNanos:     1000,
				CPUSystemNanos:   500,
				MemoryBytes:      8192,
				MemoryLimitBytes: 1 << 30,
				PIDs:             2,
			},
		})
	case guestproto.OpContainerPause:
		f.ctrMu.Lock()
		already := f.paused
		f.paused = true
		f.ctrMu.Unlock()
		if already {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeAlreadyPaused, Error: "already paused"})
			return
		}
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
	case guestproto.OpContainerUnpause:
		f.ctrMu.Lock()
		wasPaused := f.paused
		f.paused = false
		f.ctrMu.Unlock()
		if !wasPaused {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeNotPaused, Error: "not paused"})
			return
		}
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: true})
	case guestproto.OpContainerStat:
		if req.ContainerCopy.Path == "/missing" {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeNotFound, Error: "no such file"})
			return
		}
		_ = vmproto.WriteLine(conn, guestproto.Response{
			OK: true,
			PathStat: &guestproto.ContainerPathStat{
				Name: filepath.Base(req.ContainerCopy.Path),
				Size: 99,
				Mode: 0o644,
			},
		})
	case guestproto.OpContainerCopyFrom:
		if req.ContainerCopy.Path == "/missing" {
			_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeNotFound, Error: "not found"})
			return
		}
		if err := vmproto.WriteLine(conn, guestproto.Response{
			OK:       true,
			PathStat: &guestproto.ContainerPathStat{Name: "file.txt", Size: 10, Mode: 0o644},
		}); err != nil {
			return
		}
		_ = guestproto.WriteFrame(conn, guestproto.FrameData, []byte("tar-part-1:"))
		_ = guestproto.WriteFrame(conn, guestproto.FrameData, []byte("tar-part-2"))
		f.ctrMu.Lock()
		errMsg := f.copyFromErrMsg
		f.ctrMu.Unlock()
		if errMsg != "" {
			_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, []byte(errMsg))
		} else {
			_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
		}
	case guestproto.OpContainerCopyTo:
		if err := vmproto.WriteLine(conn, guestproto.Response{OK: true}); err != nil {
			return
		}
		var got bytes.Buffer
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				return
			}
			if kind == guestproto.FrameData {
				got.Write(payload)
				f.ctrMu.Lock()
				failMsg := f.copyToFailMsg
				f.ctrMu.Unlock()
				if failMsg != "" {
					_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, []byte(failMsg))
					return
				}
			} else if kind == guestproto.FrameEOF {
				f.ctrMu.Lock()
				f.copiedToPath = req.ContainerCopy.Path
				f.copiedToData = got.Bytes()
				f.ctrMu.Unlock()
				_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
				return
			}
		}
	case guestproto.OpContainerExport:
		f.ctrMu.Lock()
		if req.ContainerExport != nil {
			f.exportReqs = append(f.exportReqs, *req.ContainerExport)
		}
		f.ctrMu.Unlock()
		if err := vmproto.WriteLine(conn, guestproto.Response{OK: true}); err != nil {
			return
		}
		tag := "full"
		if req.ContainerExport != nil && req.ContainerExport.UpperOnly {
			tag = "upper"
		}
		_ = guestproto.WriteFrame(conn, guestproto.FrameData, []byte("export-"+tag))
		_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
	default:
		_ = vmproto.WriteLine(conn, guestproto.Response{OK: false, Code: guestproto.CodeUnknownOp, Error: "unknown op"})
	}
}

func TestContainerVMSpecWiringAndValidation(t *testing.T) {
	// Validate bad specs (reserved tags, path traversal, shell meta/glob chars, leading dash, duplicates).
	badSpecs := []vmproto.VMSpec{
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: ".", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "..", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "-o", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "assets", Dir: "/a"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "rosetta", Dir: "/r"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "bad:ro", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "bad,tag", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "bad/tag", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "bad*glob", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "bad?glob", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "bad[0]", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "$HOME", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "a;id", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: `a\b`, Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: `a"b`, Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "a'b", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "a=b", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "a b", Dir: "/x"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "dup", Dir: "/a"}, {Tag: "dup", Dir: "/b"}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, Shares: []vmproto.ShareSpec{{Tag: "ok", Dir: ""}}},
		{Kernel: "/k", CPUs: 1, MemoryMiB: 512, ExtraDisks: []vmproto.ExtraDiskSpec{{Path: ""}}},
	}
	for i, sp := range badSpecs {
		if err := validateSpec(sp); err == nil {
			t.Fatalf("badSpecs[%d] unexpectedly accepted: %+v", i, sp)
		}
	}
	// Valid characters [A-Za-z0-9._-] (not ".", "..", or leading "-") must pass.
	if err := validateSpec(vmproto.VMSpec{
		Kernel:    "/k",
		CPUs:      1,
		MemoryMiB: 512,
		Shares: []vmproto.ShareSpec{
			{Tag: "layers", Dir: "/l", ReadOnly: true},
			{Tag: "b_0-1.data", Dir: "/d"},
		},
	}); err != nil {
		t.Fatalf("valid share tags rejected: %v", err)
	}

	// Boot a container micro-VM without Rosetta, without Assets, and without Services,
	// even when Rosetta is NOT installed on the host.
	var gotCfg vz.Config
	dir, err := os.MkdirTemp("/tmp", "gvxd-vhc-")
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
		helperVersion: "ctr-test",
		newVM: func(cfg vz.Config) (vmInstance, error) {
			gotCfg = cfg
			return newFakeContainerVM(cfg), nil
		},
		rosettaAvail: func() vz.Availability { return vz.RosettaNotInstalled },
		agentReady:   2 * time.Second,
		agentPoll:    5 * time.Millisecond,
		logf:         func(string, ...any) {},
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

	// Hello advertises FeatureContainers.
	hello := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpHello})
	if !hello.OK || hello.Hello == nil || !hello.Hello.HasFeature(vmproto.FeatureContainers) {
		t.Fatalf("hello = %+v, want FeatureContainers", hello)
	}

	diskPath := filepath.Join(dir, "rootfs.ext4")
	spec := vmproto.VMSpec{
		Kernel:    "/assets/Image",
		Initrd:    "/assets/initramfs.gz",
		Assets:    "",
		CPUs:      4,
		MemoryMiB: 1024,
		Disk:      &vmproto.DiskSpec{Path: diskPath, Size: 8 << 20},
		Rosetta:   false,
		Network:   true,
		Services:  nil,
		Shares: []vmproto.ShareSpec{
			{Tag: "layers", Dir: "/cache/layers", ReadOnly: true},
			{Tag: "b0", Dir: "/Users/alice/src", ReadOnly: false},
		},
		ExtraDisks: []vmproto.ExtraDiskSpec{
			{Path: "/cache/l0.erofs", ReadOnly: true},
			{Path: "/volumes/vol0.ext4", ReadOnly: false},
		},
	}

	resp := sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if !resp.OK || resp.Status == nil || resp.Status.State != vmproto.Running {
		t.Fatalf("start container VM failed: %+v", resp)
	}

	// Verify vz.Config Shares, Disks, and CommandLine.
	wantShares := []vz.Share{
		{Tag: "layers", Dir: "/cache/layers", ReadOnly: true},
		{Tag: "b0", Dir: "/Users/alice/src", ReadOnly: false},
	}
	if !slices.Equal(gotCfg.Shares, wantShares) {
		t.Fatalf("vz Shares = %+v, want %+v", gotCfg.Shares, wantShares)
	}
	wantDisks := []vz.Disk{
		{Path: diskPath, ReadOnly: false},
		{Path: "/cache/l0.erofs", ReadOnly: true},
		{Path: "/volumes/vol0.ext4", ReadOnly: false},
	}
	if !slices.Equal(gotCfg.Disks, wantDisks) {
		t.Fatalf("vz Disks = %+v, want %+v", gotCfg.Disks, wantDisks)
	}
	if !strings.Contains(gotCfg.CommandLine, "gvx.shares=layers:ro,b0") {
		t.Fatalf("cmdline missing gvx.shares: %q", gotCfg.CommandLine)
	}
	if strings.Contains(gotCfg.CommandLine, "gvx.assets=") || strings.Contains(gotCfg.CommandLine, "gvx.rosetta=") {
		t.Fatalf("cmdline should not contain assets/rosetta: %q", gotCfg.CommandLine)
	}

	// Different ExtraDisks or Shares must report CodeBusy.
	other := cloneSpec(spec)
	other.ExtraDisks[0].Path = "/cache/l1.erofs"
	resp = sendReq(t, sock, vmproto.Request{
		V:     vmproto.Version,
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: other},
	})
	if resp.OK || resp.Code != vmproto.CodeBusy {
		t.Fatalf("different ExtraDisks start = %+v, want busy", resp)
	}
}

func TestServerContainerOpsProxyAndExecCLI(t *testing.T) {
	var vmMu sync.Mutex
	var fvm *fakeContainerVM
	_, sock := startTestServer(t, func(cfg vz.Config) (vmInstance, error) {
		f := newFakeContainerVM(cfg)
		vmMu.Lock()
		fvm = f
		vmMu.Unlock()
		return f, nil
	})

	// Container op before VM starts -> CodeNotRunning.
	resp := sendReq(t, sock, vmproto.Request{
		V:         vmproto.Version,
		Op:        vmproto.OpContainerStart,
		Container: &vmproto.ContainerSpec{Argv: []string{"/bin/sh"}},
	})
	if resp.OK || resp.Code != vmproto.CodeNotRunning {
		t.Fatalf("container-start before VM start = %+v, want not-running", resp)
	}

	// Start VM.
	resp = sendReq(t, sock, vmproto.Request{
		V:  vmproto.Version,
		Op: vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: vmproto.VMSpec{
			Kernel: "/k", CPUs: 2, MemoryMiB: 1024,
		}},
	})
	if !resp.OK {
		t.Fatalf("start VM: %+v", resp)
	}

	// ContainerStart.
	resp = sendReq(t, sock, vmproto.Request{
		V:  vmproto.Version,
		Op: vmproto.OpContainerStart,
		Container: &vmproto.ContainerSpec{
			Argv:   []string{"/bin/sh"},
			Layers: []vmproto.LayerSource{{Device: "/dev/vdb"}},
		},
	})
	if !resp.OK || resp.Container == nil || !resp.Container.Running || resp.Container.PID != 1 {
		t.Fatalf("container-start = %+v", resp)
	}

	// ContainerSignal & ContainerResize.
	resp = sendReq(t, sock, vmproto.Request{
		V:               vmproto.Version,
		Op:              vmproto.OpContainerSignal,
		ContainerSignal: &vmproto.ContainerSignalRequest{Signal: "SIGTERM", PID: 1},
	})
	if !resp.OK {
		t.Fatalf("container-signal = %+v", resp)
	}
	resp = sendReq(t, sock, vmproto.Request{
		V:               vmproto.Version,
		Op:              vmproto.OpContainerResize,
		ContainerResize: &vmproto.ContainerResizeRequest{Rows: 30, Cols: 100},
	})
	if !resp.OK {
		t.Fatalf("container-resize = %+v", resp)
	}

	// ContainerTop & ContainerStats.
	resp = sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerTop})
	if !resp.OK || len(resp.Processes) != 1 || resp.Processes[0].Comm != "sh" {
		t.Fatalf("container-top = %+v", resp)
	}
	resp = sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerStats})
	if !resp.OK || resp.Stats == nil || resp.Stats.MemoryBytes != 8192 {
		t.Fatalf("container-stats = %+v", resp)
	}

	// ContainerPause -> CodeAlreadyPaused -> CodePaused on exec -> ContainerUnpause -> CodeNotPaused.
	if r := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerPause}); !r.OK {
		t.Fatalf("pause: %+v", r)
	}
	if r := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerPause}); r.OK || r.Code != vmproto.CodeAlreadyPaused {
		t.Fatalf("double pause = %+v, want already-paused", r)
	}
	if r := sendReq(t, sock, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerExec,
		ContainerExec: &vmproto.ContainerExecRequest{Argv: []string{"echo"}},
	}); r.OK || r.Code != vmproto.CodePaused {
		t.Fatalf("exec while paused = %+v, want paused", r)
	}
	if r := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerUnpause}); !r.OK {
		t.Fatalf("unpause: %+v", r)
	}
	if r := sendReq(t, sock, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerUnpause}); r.OK || r.Code != vmproto.CodeNotPaused {
		t.Fatalf("double unpause = %+v, want not-paused", r)
	}

	// ContainerStat (found + not-found).
	if r := sendReq(t, sock, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerStat,
		ContainerCopy: &vmproto.ContainerCopyRequest{Path: "/etc/hosts"},
	}); !r.OK || r.PathStat == nil || r.PathStat.Name != "hosts" {
		t.Fatalf("stat = %+v", r)
	}
	if r := sendReq(t, sock, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerStat,
		ContainerCopy: &vmproto.ContainerCopyRequest{Path: "/missing"},
	}); r.OK || r.Code != vmproto.CodeNotFound {
		t.Fatalf("stat missing = %+v, want not-found", r)
	}

	// __vm-helper exec subcommand (execWithIO) with interactive stdin, env, user, workdir, rows/cols.
	var stdout, stderr bytes.Buffer
	code := execWithIO(
		context.Background(),
		[]string{
			"-socket", sock,
			"-i", "-t",
			"-u", "1000:1000",
			"-w", "/work",
			"-e", "A=1", "-e", "B=2",
			"-privileged",
			"-rows", "40", "-cols", "120",
			"--", "hello", "world",
		},
		strings.NewReader("stdin-data"),
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("execWithIO exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "hello world|stdin-data" {
		t.Fatalf("execWithIO stdout = %q, want %q", stdout.String(), "hello world|stdin-data")
	}
	if stderr.String() != "exec-err" {
		t.Fatalf("execWithIO stderr = %q, want %q", stderr.String(), "exec-err")
	}

	// Non-zero exit code propagation via execWithIO.
	stdout.Reset()
	stderr.Reset()
	code = execWithIO(context.Background(), []string{"-socket", sock, "--", "fail42"}, nil, &stdout, &stderr)
	if code != 42 {
		t.Fatalf("execWithIO fail42 exit = %d, want 42", code)
	}

	// Missing flags return usage exit code 2.
	stderr.Reset()
	if code := execWithIO(context.Background(), []string{"--", "sh"}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing -socket exit = %d, want 2", code)
	}
	if code := execWithIO(context.Background(), []string{"-socket", sock}, nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing argv exit = %d, want 2", code)
	}

	// Verify recorded exec request fields on guest side.
	vmMu.Lock()
	if len(fvm.execReqs) < 1 {
		vmMu.Unlock()
		t.Fatal("expected recorded exec request")
	}
	gotExec := fvm.execReqs[0]
	vmMu.Unlock()
	if gotExec.User != "1000:1000" || gotExec.Workdir != "/work" || !gotExec.TTY || !gotExec.Interactive || !gotExec.Privileged ||
		gotExec.Rows != 40 || gotExec.Cols != 120 || !slices.Equal(gotExec.Env, []string{"A=1", "B=2"}) {
		t.Fatalf("recorded exec req = %+v", gotExec)
	}

	// ContainerAttach streaming round-trip through vmhelper proxy.
	attConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := vmproto.WriteLine(attConn, vmproto.Request{V: vmproto.Version, Op: vmproto.OpContainerAttach}); err != nil {
		t.Fatal(err)
	}
	attBr := bufio.NewReader(attConn)
	var attResp vmproto.Response
	if err := vmproto.ReadLine(attBr, &attResp); err != nil || !attResp.OK || attResp.Container == nil || attResp.Container.PID != 1 {
		t.Fatalf("attach handshake: resp=%+v err=%v", attResp, err)
	}
	rzPayload, _ := json.Marshal(guestproto.ContainerResizeRequest{Rows: 28, Cols: 95})
	_ = guestproto.WriteFrame(attConn, guestproto.FrameResize, rzPayload)
	_ = guestproto.WriteFrame(attConn, guestproto.FrameStdin, []byte("hi"))
	_ = guestproto.WriteFrame(attConn, guestproto.FrameStdinEOF, nil)
	var attOut, attErr bytes.Buffer
	var sawExit bool
	for !sawExit {
		kind, payload, rerr := guestproto.ReadFrame(attBr)
		if rerr != nil {
			t.Fatalf("attach read frame: %v", rerr)
		}
		switch kind {
		case guestproto.FrameStdout:
			attOut.Write(payload)
		case guestproto.FrameStderr:
			attErr.Write(payload)
		case guestproto.FrameExit:
			var ex guestproto.Exit
			if err := json.Unmarshal(payload, &ex); err != nil || ex.Code != 0 {
				t.Fatalf("attach exit = (%+v, %v)", ex, err)
			}
			sawExit = true
		}
	}
	_ = attConn.Close()
	if attOut.String() != "booted\nin:hi" || attErr.String() != "warn\n" {
		t.Fatalf("attach out=%q err=%q", attOut.String(), attErr.String())
	}

	// ContainerCopyFrom through vmhelper proxy.
	cfConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = vmproto.WriteLine(cfConn, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerCopyFrom,
		ContainerCopy: &vmproto.ContainerCopyRequest{Path: "/etc/hosts"},
	})
	cfBr := bufio.NewReader(cfConn)
	var cfResp vmproto.Response
	if err := vmproto.ReadLine(cfBr, &cfResp); err != nil || !cfResp.OK || cfResp.PathStat == nil {
		t.Fatalf("copy-from handshake: resp=%+v err=%v", cfResp, err)
	}
	var cfData bytes.Buffer
	for {
		kind, payload, rerr := guestproto.ReadFrame(cfBr)
		if rerr != nil {
			t.Fatalf("copy-from read: %v", rerr)
		}
		if kind == guestproto.FrameData {
			cfData.Write(payload)
		} else if kind == guestproto.FrameEOF {
			if len(payload) != 0 {
				t.Fatalf("copy-from unexpected error EOF: %q", payload)
			}
			break
		}
	}
	_ = cfConn.Close()
	if cfData.String() != "tar-part-1:tar-part-2" {
		t.Fatalf("copy-from data = %q", cfData.String())
	}

	// ContainerCopyTo through vmhelper proxy.
	ctConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = vmproto.WriteLine(ctConn, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerCopyTo,
		ContainerCopy: &vmproto.ContainerCopyRequest{Path: "/tmp"},
	})
	ctBr := bufio.NewReader(ctConn)
	var ctResp vmproto.Response
	if err := vmproto.ReadLine(ctBr, &ctResp); err != nil || !ctResp.OK {
		t.Fatalf("copy-to handshake: resp=%+v err=%v", ctResp, err)
	}
	_ = guestproto.WriteFrame(ctConn, guestproto.FrameData, []byte("tar-a-"))
	_ = guestproto.WriteFrame(ctConn, guestproto.FrameData, []byte("tar-b"))
	_ = guestproto.WriteFrame(ctConn, guestproto.FrameEOF, nil)
	kind, payload, err := guestproto.ReadFrame(ctBr)
	_ = ctConn.Close()
	if err != nil || kind != guestproto.FrameEOF || len(payload) != 0 {
		t.Fatalf("copy-to ack = (%d, %q, %v)", kind, payload, err)
	}
	vmMu.Lock()
	fvm.ctrMu.Lock()
	gotCTPath := fvm.copiedToPath
	gotCTData := string(fvm.copiedToData)
	fvm.ctrMu.Unlock()
	vmMu.Unlock()
	if gotCTPath != "/tmp" || gotCTData != "tar-a-tar-b" {
		t.Fatalf("copy-to guest received (%q, %q)", gotCTPath, gotCTData)
	}

	// ContainerExport (upperOnly=true) through vmhelper proxy.
	expConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = vmproto.WriteLine(expConn, vmproto.Request{
		V:               vmproto.Version,
		Op:              vmproto.OpContainerExport,
		ContainerExport: &vmproto.ContainerExportRequest{UpperOnly: true},
	})
	expBr := bufio.NewReader(expConn)
	var expResp vmproto.Response
	if err := vmproto.ReadLine(expBr, &expResp); err != nil || !expResp.OK {
		t.Fatalf("export handshake: resp=%+v err=%v", expResp, err)
	}
	var expData bytes.Buffer
	for {
		k, p, rerr := guestproto.ReadFrame(expBr)
		if rerr != nil {
			t.Fatalf("export read: %v", rerr)
		}
		if k == guestproto.FrameData {
			expData.Write(p)
		} else if k == guestproto.FrameEOF {
			if len(p) != 0 {
				t.Fatalf("export unexpected error: %q", p)
			}
			break
		}
	}
	_ = expConn.Close()
	if expData.String() != "export-upper" {
		t.Fatalf("export data = %q", expData.String())
	}

	// Verify active container-exec stream is torn down cleanly on OpStop.
	holdConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer holdConn.Close()
	if err := vmproto.WriteLine(holdConn, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerExec,
		ContainerExec: &vmproto.ContainerExecRequest{Argv: []string{"hold"}},
	}); err != nil {
		t.Fatal(err)
	}
	hbr := bufio.NewReader(holdConn)
	var hresp vmproto.Response
	if err := vmproto.ReadLine(hbr, &hresp); err != nil || !hresp.OK {
		t.Fatalf("hold exec handshake: resp=%+v err=%v", hresp, err)
	}
	kind, payload, err = guestproto.ReadFrame(hbr)
	if err != nil || kind != guestproto.FrameStdout || string(payload) != "holding" {
		t.Fatalf("hold frame = (%d, %q, %v)", kind, payload, err)
	}

	stopResp := sendReq(t, sock, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpStop,
		Stop: &vmproto.StopRequest{Timeout: vmproto.Duration(2 * time.Second)},
	})
	if !stopResp.OK || stopResp.Status.State != vmproto.Stopped {
		t.Fatalf("stop = %+v", stopResp)
	}
	_ = holdConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := guestproto.ReadFrame(hbr); err == nil {
		t.Fatal("expected active container stream to close on VM stop")
	}
}
