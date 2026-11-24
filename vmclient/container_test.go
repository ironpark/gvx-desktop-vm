package vmclient_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

type fakeContainerServer struct {
	sock string
	ln   net.Listener

	mu             sync.Mutex
	resizes        []vmproto.ContainerResizeRequest
	signals        []vmproto.ContainerSignalRequest
	paused         bool
	copiedTo       []byte
	copiedToPath   string
	copyToFail     string
	copyFromFail   string
	copyFromTrunc  bool
	exportUpperHit bool
	wg             sync.WaitGroup
}

func startFakeContainerServer(t *testing.T) *fakeContainerServer {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gvxd-vcc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, vmhelper.SocketName)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeContainerServer{sock: sock, ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func(c net.Conn) {
				defer s.wg.Done()
				s.handle(c)
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeContainerServer) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	var req vmproto.Request
	if err := vmproto.ReadLine(br, &req); err != nil {
		return
	}
	switch req.Op {
	case vmproto.OpHello:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK: true,
			Hello: &vmproto.Hello{
				Protocol:    vmproto.Version,
				MinProtocol: vmproto.Version,
				Helper:      "fake-ctr",
				PID:         os.Getpid(),
				Features:    []string{vmproto.FeatureContainers},
			},
		})
	case vmproto.OpContainerStart:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:        true,
			Container: &vmproto.ContainerState{Running: true, PID: 1},
		})
	case vmproto.OpContainerAttach:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:        true,
			Container: &vmproto.ContainerState{Running: true, PID: 1},
		})
		_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, []byte("hello-stdout:"))
		_ = guestproto.WriteFrame(conn, guestproto.FrameStderr, []byte("hello-stderr"))
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				return
			}
			switch kind {
			case guestproto.FrameStdin:
				_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, payload)
			case guestproto.FrameResize:
				var rz vmproto.ContainerResizeRequest
				if json.Unmarshal(payload, &rz) == nil {
					s.mu.Lock()
					s.resizes = append(s.resizes, rz)
					s.mu.Unlock()
				}
			case guestproto.FrameStdinEOF:
				ex, _ := json.Marshal(vmproto.ContainerExit{Code: 7, Signal: "SIGTERM", Elapsed: 99})
				_ = guestproto.WriteFrame(conn, guestproto.FrameExit, ex)
				return
			}
		}
	case vmproto.OpContainerSignal:
		s.mu.Lock()
		s.signals = append(s.signals, *req.ContainerSignal)
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
	case vmproto.OpContainerResize:
		s.mu.Lock()
		s.resizes = append(s.resizes, *req.ContainerResize)
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
	case vmproto.OpContainerExec:
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true, ExecPID: 123})
		if len(req.ContainerExec.Argv) > 0 && req.ContainerExec.Argv[0] == "block" {
			_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, []byte("running"))
			_, _, _ = guestproto.ReadFrame(br)
			return
		}
		if len(req.ContainerExec.Argv) > 0 && req.ContainerExec.Argv[0] == "stderr-only-unread" {
			_ = guestproto.WriteFrame(conn, guestproto.FrameStderr, []byte("unconsumed-stderr"))
			_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, []byte("ok-stdout"))
			ex, _ := json.Marshal(vmproto.ContainerExit{Code: 0})
			_ = guestproto.WriteFrame(conn, guestproto.FrameExit, ex)
			return
		}
		var in bytes.Buffer
		if req.ContainerExec.Interactive {
			for {
				kind, payload, err := guestproto.ReadFrame(br)
				if err != nil {
					return
				}
				switch kind {
				case guestproto.FrameStdin:
					in.Write(payload)
				case guestproto.FrameResize:
					var rz vmproto.ContainerResizeRequest
					if json.Unmarshal(payload, &rz) == nil {
						s.mu.Lock()
						s.resizes = append(s.resizes, rz)
						s.mu.Unlock()
					}
				case guestproto.FrameStdinEOF:
					goto execReply
				}
			}
		}
	execReply:
		_ = guestproto.WriteFrame(conn, guestproto.FrameStdout, append([]byte("exec:"), in.Bytes()...))
		_ = guestproto.WriteFrame(conn, guestproto.FrameStderr, []byte("err"))
		ex, _ := json.Marshal(vmproto.ContainerExit{Code: 3})
		_ = guestproto.WriteFrame(conn, guestproto.FrameExit, ex)
	case vmproto.OpContainerTop:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:        true,
			Processes: []vmproto.ContainerProcess{{PID: 1, Comm: "init"}},
		})
	case vmproto.OpContainerStats:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:    true,
			Stats: &vmproto.ContainerStats{MemoryBytes: 4096, PIDs: 1},
		})
	case vmproto.OpContainerPause:
		s.mu.Lock()
		s.paused = true
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
	case vmproto.OpContainerUnpause:
		s.mu.Lock()
		s.paused = false
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
	case vmproto.OpContainerStat:
		_ = vmproto.WriteLine(conn, vmproto.Response{
			OK:       true,
			PathStat: &vmproto.ContainerPathStat{Name: "app", Size: 12, Mode: 0o755},
		})
	case vmproto.OpContainerCopyFrom:
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
		_ = guestproto.WriteFrame(conn, guestproto.FrameData, []byte("chunk1-"))
		_ = guestproto.WriteFrame(conn, guestproto.FrameData, []byte("chunk2"))
		s.mu.Lock()
		fail := s.copyFromFail
		trunc := s.copyFromTrunc
		s.mu.Unlock()
		if trunc {
			return // drop connection without FrameEOF
		}
		if fail != "" {
			_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, []byte(fail))
			return
		}
		_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
	case vmproto.OpContainerCopyTo:
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
		var buf bytes.Buffer
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				return
			}
			if kind == guestproto.FrameData {
				buf.Write(payload)
				s.mu.Lock()
				fail := s.copyToFail
				s.mu.Unlock()
				if fail != "" {
					_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, []byte(fail))
					return
				}
			} else if kind == guestproto.FrameEOF {
				s.mu.Lock()
				s.copiedToPath = req.ContainerCopy.Path
				s.copiedTo = buf.Bytes()
				s.mu.Unlock()
				_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
				return
			}
		}
	case vmproto.OpContainerExport:
		s.mu.Lock()
		s.exportUpperHit = req.ContainerExport != nil && req.ContainerExport.UpperOnly
		s.mu.Unlock()
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
		_ = guestproto.WriteFrame(conn, guestproto.FrameData, []byte("export-tar-bytes"))
		_ = guestproto.WriteFrame(conn, guestproto.FrameEOF, nil)
	case vmproto.OpContainerNetStream:
		if req.ContainerNet == nil {
			_ = vmproto.WriteLine(conn, vmproto.Response{OK: false, Code: vmproto.CodeBadRequest, Error: "missing containerNet"})
			return
		}
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
		// Echo back packets and DNS replies as frames so client methods are verified.
		for {
			kind, payload, err := guestproto.ReadFrame(br)
			if err != nil {
				return
			}
			_ = guestproto.WriteFrame(conn, kind, payload)
		}
	case vmproto.OpContainerNetUpdate:
		if req.ContainerNet == nil {
			_ = vmproto.WriteLine(conn, vmproto.Response{OK: false, Code: vmproto.CodeBadRequest, Error: "missing containerNet"})
			return
		}
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: true})
	default:
		_ = vmproto.WriteLine(conn, vmproto.Response{OK: false, Code: vmproto.CodeUnknownOp})
	}
}

func TestClientContainerMethods(t *testing.T) {
	srv := startFakeContainerServer(t)
	client := vmclient.Connect(srv.sock)
	// Close on a connected client must be a safe no-op.
	defer client.Close(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Hello advertises containers feature.
	hello, err := client.Hello(ctx)
	if err != nil || !hello.HasFeature(vmproto.FeatureContainers) {
		t.Fatalf("Hello = %+v, err=%v", hello, err)
	}

	// ContainerStart.
	st, err := client.ContainerStart(ctx, vmproto.ContainerSpec{Argv: []string{"/bin/sh"}})
	if err != nil || !st.Running || st.PID != 1 {
		t.Fatalf("ContainerStart = %+v, err=%v", st, err)
	}

	// ContainerAttach: stdin, resize, stdout, stderr, exit.
	att, err := client.ContainerAttach(ctx)
	if err != nil {
		t.Fatalf("ContainerAttach: %v", err)
	}
	if att.PID() != 1 || att.State() == nil || !att.State().Running {
		t.Fatalf("attach metadata: pid=%d state=%+v", att.PID(), att.State())
	}
	if err := att.Resize(25, 90); err != nil {
		t.Fatalf("attach Resize: %v", err)
	}
	if _, err := att.Stdin().Write([]byte("ping")); err != nil {
		t.Fatalf("attach Stdin Write: %v", err)
	}
	if err := att.Stdin().Close(); err != nil {
		t.Fatalf("attach Stdin Close: %v", err)
	}
	// Second CloseStdin is idempotent; WriteStdin after CloseStdin fails.
	if err := att.CloseStdin(); err != nil {
		t.Fatalf("second CloseStdin: %v", err)
	}
	if _, err := att.WriteStdin([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("WriteStdin after close = %v, want ErrClosedPipe", err)
	}
	ex, err := att.Wait()
	if err != nil || ex.Code != 7 || ex.Signal != "SIGTERM" {
		t.Fatalf("attach Wait = (%+v, %v)", ex, err)
	}
	outBytes, err := io.ReadAll(att.Stdout())
	if err != nil || string(outBytes) != "hello-stdout:ping" {
		t.Fatalf("attach Stdout = %q, err=%v", outBytes, err)
	}
	errBytes, err := io.ReadAll(att.Stderr())
	if err != nil || string(errBytes) != "hello-stderr" {
		t.Fatalf("attach Stderr = %q, err=%v", errBytes, err)
	}
	if err := att.Close(); err != nil {
		t.Fatalf("attach Close: %v", err)
	}

	// ContainerSignal & ContainerResize.
	if err := client.ContainerSignal(ctx, vmproto.ContainerSignalRequest{Signal: "HUP", PID: 1}); err != nil {
		t.Fatalf("ContainerSignal: %v", err)
	}
	if err := client.ContainerResize(ctx, 50, 140); err != nil {
		t.Fatalf("ContainerResize: %v", err)
	}

	// ContainerExec: interactive stdin + resize + stdout/stderr + exit.
	execStream, err := client.ContainerExec(ctx, vmproto.ContainerExecRequest{
		Argv:        []string{"cat"},
		Interactive: true,
		TTY:         true,
	})
	if err != nil {
		t.Fatalf("ContainerExec: %v", err)
	}
	if execStream.PID() != 123 {
		t.Fatalf("exec PID = %d, want 123", execStream.PID())
	}
	if err := execStream.Resize(30, 100); err != nil {
		t.Fatalf("exec Resize: %v", err)
	}
	_, _ = execStream.WriteStdin([]byte("abc"))
	_ = execStream.CloseStdin()
	ex, err = execStream.Wait()
	if err != nil || ex.Code != 3 {
		t.Fatalf("exec Wait = (%+v, %v)", ex, err)
	}
	gotOut, _ := io.ReadAll(execStream.Stdout())
	gotErr, _ := io.ReadAll(execStream.Stderr())
	if string(gotOut) != "exec:abc" || string(gotErr) != "err" {
		t.Fatalf("exec out=%q err=%q", gotOut, gotErr)
	}
	_ = execStream.Close()

	// Unread stderr does not deadlock Wait or Stdout.
	unreadStream, err := client.ContainerExec(ctx, vmproto.ContainerExecRequest{Argv: []string{"stderr-only-unread"}})
	if err != nil {
		t.Fatalf("exec unread: %v", err)
	}
	if ex, err := unreadStream.Wait(); err != nil || ex.Code != 0 {
		t.Fatalf("unread Wait = (%+v, %v)", ex, err)
	}
	gotOut, _ = io.ReadAll(unreadStream.Stdout())
	if string(gotOut) != "ok-stdout" {
		t.Fatalf("unread stdout = %q", gotOut)
	}
	_ = unreadStream.Close()

	// Early Close on a blocking exec detaches cleanly without hanging.
	blockStream, err := client.ContainerExec(ctx, vmproto.ContainerExecRequest{Argv: []string{"block"}})
	if err != nil {
		t.Fatalf("exec block: %v", err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(blockStream.Stdout(), buf); err != nil || string(buf) != "running" {
		t.Fatalf("block stdout = %q, err=%v", buf, err)
	}
	_ = blockStream.Close()
	if _, err := blockStream.Wait(); err == nil {
		t.Fatal("expected non-nil Wait error after early Close")
	}

	// ContainerTop, ContainerStats, ContainerPause, ContainerUnpause, ContainerStat.
	procs, err := client.ContainerTop(ctx)
	if err != nil || len(procs) != 1 || procs[0].Comm != "init" {
		t.Fatalf("ContainerTop = %+v, err=%v", procs, err)
	}
	stats, err := client.ContainerStats(ctx)
	if err != nil || stats.MemoryBytes != 4096 || stats.PIDs != 1 {
		t.Fatalf("ContainerStats = %+v, err=%v", stats, err)
	}
	if err := client.ContainerPause(ctx); err != nil {
		t.Fatalf("ContainerPause: %v", err)
	}
	if err := client.ContainerUnpause(ctx); err != nil {
		t.Fatalf("ContainerUnpause: %v", err)
	}
	ps, err := client.ContainerStat(ctx, vmproto.ContainerCopyRequest{Path: "/app"})
	if err != nil || ps.Name != "app" || ps.Size != 12 {
		t.Fatalf("ContainerStat = %+v, err=%v", ps, err)
	}

	// ContainerCopyFrom: success (small read buffer exercises partial frame reads).
	rc, err := client.ContainerCopyFrom(ctx, vmproto.ContainerCopyRequest{Path: "/app"})
	if err != nil {
		t.Fatalf("ContainerCopyFrom: %v", err)
	}
	var cpBuf bytes.Buffer
	small := make([]byte, 3)
	for {
		n, rerr := rc.Read(small)
		cpBuf.Write(small[:n])
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("CopyFrom Read: %v", rerr)
		}
	}
	_ = rc.Close()
	if cpBuf.String() != "chunk1-chunk2" {
		t.Fatalf("CopyFrom data = %q", cpBuf.String())
	}

	// ContainerCopyFrom: mid-stream guest error via FrameEOF payload.
	srv.mu.Lock()
	srv.copyFromFail = "disk read error"
	srv.mu.Unlock()
	rc, err = client.ContainerCopyFrom(ctx, vmproto.ContainerCopyRequest{Path: "/app"})
	if err != nil {
		t.Fatalf("ContainerCopyFrom fail open: %v", err)
	}
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	if err == nil || !strings.Contains(err.Error(), "disk read error") {
		t.Fatalf("CopyFrom mid-stream err = %v, want disk read error", err)
	}

	// ContainerCopyFrom: abrupt connection drop without FrameEOF -> ErrUnexpectedEOF.
	srv.mu.Lock()
	srv.copyFromFail = ""
	srv.copyFromTrunc = true
	srv.mu.Unlock()
	rc, err = client.ContainerCopyFrom(ctx, vmproto.ContainerCopyRequest{Path: "/app"})
	if err != nil {
		t.Fatalf("ContainerCopyFrom trunc open: %v", err)
	}
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("CopyFrom truncated err = %v, want ErrUnexpectedEOF", err)
	}

	// ContainerCopyTo: large payload (> 64 KiB chunk size) success.
	bigPayload := bytes.Repeat([]byte("tar-archive-data-"), 5000)
	if err := client.ContainerCopyTo(ctx, vmproto.ContainerCopyRequest{Path: "/dst"}, bytes.NewReader(bigPayload)); err != nil {
		t.Fatalf("ContainerCopyTo: %v", err)
	}
	srv.mu.Lock()
	gotPath := srv.copiedToPath
	gotCopied := append([]byte(nil), srv.copiedTo...)
	srv.mu.Unlock()
	if gotPath != "/dst" || !bytes.Equal(gotCopied, bigPayload) {
		t.Fatalf("CopyTo path=%q len=%d, want /dst len=%d", gotPath, len(gotCopied), len(bigPayload))
	}

	// ContainerCopyTo: mid-stream guest extraction failure surfaced cleanly.
	srv.mu.Lock()
	srv.copyToFail = "path traversal rejected"
	srv.mu.Unlock()
	err = client.ContainerCopyTo(ctx, vmproto.ContainerCopyRequest{Path: "/dst"}, bytes.NewReader(bigPayload))
	if err == nil || !strings.Contains(err.Error(), "path traversal rejected") {
		t.Fatalf("CopyTo error = %v, want path traversal rejected", err)
	}

	// ContainerExport: UpperOnly true.
	expRC, err := client.ContainerExport(ctx, vmproto.ContainerExportRequest{UpperOnly: true})
	if err != nil {
		t.Fatalf("ContainerExport: %v", err)
	}
	expData, err := io.ReadAll(expRC)
	_ = expRC.Close()
	if err != nil || string(expData) != "export-tar-bytes" {
		t.Fatalf("ContainerExport = %q, err=%v", expData, err)
	}
	srv.mu.Lock()
	upperHit := srv.exportUpperHit
	srv.mu.Unlock()
	if !upperHit {
		t.Fatal("expected UpperOnly=true on export")
	}

	// ContainerNetAttach & ContainerNetUpdate.
	netStream, err := client.ContainerNetAttach(ctx, vmproto.ContainerNetConfig{
		Addresses: []string{"10.89.1.2/24"},
		Routes:    []string{"10.89.0.0/16"},
		MTU:       60000,
		DNS:       true,
	})
	if err != nil {
		t.Fatalf("ContainerNetAttach: %v", err)
	}
	pkt := make([]byte, 24)
	pkt[0] = 0x45
	if err := netStream.SendPacket(pkt); err != nil {
		t.Fatalf("SendPacket: %v", err)
	}
	kind, gotPkt, err := netStream.ReadFrame()
	if err != nil || kind != guestproto.FrameNetPacket || !bytes.Equal(gotPkt, pkt) {
		t.Fatalf("ReadFrame pkt = (%d, %x, %v)", kind, gotPkt, err)
	}
	if err := netStream.SendDNSReply(99, []byte{0xde, 0xad}); err != nil {
		t.Fatalf("SendDNSReply: %v", err)
	}
	kind, gotDNS, err := netStream.ReadFrame()
	if err != nil || kind != guestproto.FrameDNSReply || !bytes.Equal(gotDNS, []byte{0, 0, 0, 99, 0xde, 0xad}) {
		t.Fatalf("ReadFrame dns = (%d, %x, %v)", kind, gotDNS, err)
	}
	if err := client.ContainerNetUpdate(ctx, vmproto.ContainerNetConfig{
		Addresses: []string{"10.89.1.2/24", "10.89.2.2/24"},
		Routes:    []string{"10.89.0.0/16"},
	}); err != nil {
		t.Fatalf("ContainerNetUpdate: %v", err)
	}
	if err := netStream.Close(); err != nil {
		t.Fatalf("netStream.Close: %v", err)
	}
}
