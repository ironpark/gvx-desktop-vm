package vmclient

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

const (
	containerPipeLimit = 4 << 20 // 4 MiB buffered per stdout/stderr stream
	copyChunkSize      = 64 * 1024
)

// ContainerStart mounts image layers and mounts inside the guest and starts
// the micro-VM's container PID 1.
func (c *Client) ContainerStart(ctx context.Context, spec vmproto.ContainerSpec) (vmproto.ContainerState, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{
		Op:        vmproto.OpContainerStart,
		Container: &spec,
	})
	if err != nil {
		return vmproto.ContainerState{}, err
	}
	if resp.Container == nil {
		return vmproto.ContainerState{}, nil
	}
	return *resp.Container, nil
}

// ContainerAttach opens a live bidirectional stream to the container's PID 1
// stdio. ctx bounds opening the stream; callers own the returned stream's
// lifecycle via Close.
func (c *Client) ContainerAttach(ctx context.Context) (*ContainerStream, error) {
	conn, br, resp, err := c.openStream(ctx, vmproto.Request{
		Op: vmproto.OpContainerAttach,
	})
	if err != nil {
		return nil, err
	}
	pid := 0
	if resp.Container != nil {
		pid = resp.Container.PID
	}
	return newContainerStream(conn, br, pid, resp.Container), nil
}

// ContainerSignal delivers a signal inside the running container.
func (c *Client) ContainerSignal(ctx context.Context, req vmproto.ContainerSignalRequest) error {
	_, err := c.roundTrip(ctx, vmproto.Request{
		Op:              vmproto.OpContainerSignal,
		ContainerSignal: &req,
	})
	return err
}

// ContainerResize updates container PID 1's PTY window size.
func (c *Client) ContainerResize(ctx context.Context, rows, cols uint16) error {
	_, err := c.roundTrip(ctx, vmproto.Request{
		Op:              vmproto.OpContainerResize,
		ContainerResize: &vmproto.ContainerResizeRequest{Rows: rows, Cols: cols},
	})
	return err
}

// ContainerExec runs a process inside the running container and returns its
// bidirectional framed stream. ctx bounds opening the stream; callers own the
// returned stream's lifecycle via Close.
func (c *Client) ContainerExec(ctx context.Context, req vmproto.ContainerExecRequest) (*ContainerStream, error) {
	conn, br, resp, err := c.openStream(ctx, vmproto.Request{
		Op:            vmproto.OpContainerExec,
		ContainerExec: &req,
	})
	if err != nil {
		return nil, err
	}
	return newContainerStream(conn, br, resp.ExecPID, nil), nil
}

// ContainerTop lists running processes inside the container.
func (c *Client) ContainerTop(ctx context.Context) ([]vmproto.ContainerProcess, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{
		Op: vmproto.OpContainerTop,
	})
	if err != nil {
		return nil, err
	}
	return resp.Processes, nil
}

// ContainerStats returns a point-in-time resource usage snapshot for the container.
func (c *Client) ContainerStats(ctx context.Context) (vmproto.ContainerStats, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{
		Op: vmproto.OpContainerStats,
	})
	if err != nil {
		return vmproto.ContainerStats{}, err
	}
	if resp.Stats == nil {
		return vmproto.ContainerStats{}, nil
	}
	return *resp.Stats, nil
}

// ContainerPause freezes the container's cgroup.
func (c *Client) ContainerPause(ctx context.Context) error {
	_, err := c.roundTrip(ctx, vmproto.Request{Op: vmproto.OpContainerPause})
	return err
}

// ContainerUnpause thaws a paused container's cgroup.
func (c *Client) ContainerUnpause(ctx context.Context) error {
	_, err := c.roundTrip(ctx, vmproto.Request{Op: vmproto.OpContainerUnpause})
	return err
}

// ContainerStat inspects a path inside the container rootfs.
func (c *Client) ContainerStat(ctx context.Context, req vmproto.ContainerCopyRequest) (vmproto.ContainerPathStat, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{
		Op:            vmproto.OpContainerStat,
		ContainerCopy: &req,
	})
	if err != nil {
		return vmproto.ContainerPathStat{}, err
	}
	if resp.PathStat == nil {
		return vmproto.ContainerPathStat{}, errors.New("vmclient: missing pathStat payload")
	}
	return *resp.PathStat, nil
}

// ContainerCopyFrom streams a tar archive of req.Path out of the container.
// ctx bounds opening the stream; callers must close the returned reader.
func (c *Client) ContainerCopyFrom(ctx context.Context, req vmproto.ContainerCopyRequest) (io.ReadCloser, error) {
	conn, br, _, err := c.openStream(ctx, vmproto.Request{
		Op:            vmproto.OpContainerCopyFrom,
		ContainerCopy: &req,
	})
	if err != nil {
		return nil, err
	}
	return &frameDataReader{conn: conn, br: br}, nil
}

// ContainerCopyTo streams the tar archive from r into the container directory
// req.Path and waits for the guest to finish extracting.
func (c *Client) ContainerCopyTo(ctx context.Context, req vmproto.ContainerCopyRequest, r io.Reader) error {
	conn, err := c.dialControl(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	if err := vmproto.WriteLine(conn, vmproto.Request{
		V:             vmproto.Version,
		Op:            vmproto.OpContainerCopyTo,
		ContainerCopy: &req,
	}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	br := bufio.NewReader(conn)
	var resp vmproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if !resp.OK {
		return resp.Err()
	}

	// Read the guest's completion FrameEOF concurrently so mid-stream guest
	// errors are captured even if writing fails with broken pipe.
	resCh := make(chan error, 1)
	go func() {
		for {
			kind, payload, rerr := guestproto.ReadFrame(br)
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					resCh <- io.ErrUnexpectedEOF
				} else {
					resCh <- rerr
				}
				return
			}
			if kind == guestproto.FrameEOF {
				if len(payload) > 0 {
					resCh <- &vmproto.Error{Code: vmproto.CodeGuest, Message: string(payload)}
				} else {
					resCh <- nil
				}
				return
			}
		}
	}()

	var writeErr error
	if r != nil {
		buf := make([]byte, copyChunkSize)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				if werr := guestproto.WriteFrame(conn, guestproto.FrameData, buf[:n]); werr != nil {
					writeErr = werr
					break
				}
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					_ = conn.Close()
					<-resCh
					return rerr
				}
				break
			}
		}
	}
	if writeErr == nil {
		if werr := guestproto.WriteFrame(conn, guestproto.FrameEOF, nil); werr != nil {
			writeErr = werr
		}
	}

	guestErr := <-resCh
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Prefer a structured guest error (e.g. path traversal or extraction failure)
	// over a secondary write error like broken pipe.
	var protoErr *vmproto.Error
	if errors.As(guestErr, &protoErr) {
		return guestErr
	}
	if writeErr != nil {
		return writeErr
	}
	return guestErr
}

// ContainerExport streams a tar archive of the container rootfs (or only the
// writable upper layer when req.UpperOnly is true). ctx bounds opening the
// stream; callers must close the returned reader.
func (c *Client) ContainerExport(ctx context.Context, req vmproto.ContainerExportRequest) (io.ReadCloser, error) {
	conn, br, _, err := c.openStream(ctx, vmproto.Request{
		Op:              vmproto.OpContainerExport,
		ContainerExport: &req,
	})
	if err != nil {
		return nil, err
	}
	return &frameDataReader{conn: conn, br: br}, nil
}

// ContainerNetAttach opens the bidirectional overlay network & DNS relay
// stream (gvx0 TUN + 127.0.0.11:53) inside the guest. ctx bounds opening the
// stream; callers own the returned stream's lifecycle via Close.
func (c *Client) ContainerNetAttach(ctx context.Context, cfg vmproto.ContainerNetConfig) (*ContainerNetStream, error) {
	conn, br, _, err := c.openStream(ctx, vmproto.Request{
		Op:           vmproto.OpContainerNetStream,
		ContainerNet: &cfg,
	})
	if err != nil {
		return nil, err
	}
	return &ContainerNetStream{conn: conn, br: br}, nil
}

// ContainerNetUpdate reconciles gvx0's addresses and routes on a running
// micro-VM container (for live docker network connect/disconnect).
func (c *Client) ContainerNetUpdate(ctx context.Context, cfg vmproto.ContainerNetConfig) error {
	_, err := c.roundTrip(ctx, vmproto.Request{
		Op:           vmproto.OpContainerNetUpdate,
		ContainerNet: &cfg,
	})
	return err
}

// ContainerNetStream is a live bidirectional overlay network and DNS relay
// session returned by ContainerNetAttach.
type ContainerNetStream struct {
	conn      net.Conn
	br        *bufio.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// ReadFrame reads the next guest->host frame (FrameNetPacket or FrameDNSQuery).
func (s *ContainerNetStream) ReadFrame() (kind byte, payload []byte, err error) {
	return guestproto.ReadFrame(s.br)
}

// SendPacket delivers a raw IPv4 packet to the guest's gvx0 TUN interface.
func (s *ContainerNetStream) SendPacket(pkt []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return guestproto.WriteFrame(s.conn, guestproto.FrameNetPacket, pkt)
}

// SendDNSReply delivers a raw DNS wire reply correlated with seq to the guest.
func (s *ContainerNetStream) SendDNSReply(seq uint32, reply []byte) error {
	buf := make([]byte, 4+len(reply))
	binary.BigEndian.PutUint32(buf[0:4], seq)
	copy(buf[4:], reply)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return guestproto.WriteFrame(s.conn, guestproto.FrameDNSReply, buf)
}

// Close closes the overlay network stream and tears down the guest's tunnel.
func (s *ContainerNetStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

func (c *Client) openStream(ctx context.Context, req vmproto.Request) (net.Conn, *bufio.Reader, vmproto.Response, error) {
	conn, err := c.dialControl(ctx)
	if err != nil {
		return nil, nil, vmproto.Response{}, err
	}
	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	req.V = vmproto.Version
	if err := vmproto.WriteLine(conn, req); err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, nil, vmproto.Response{}, ctx.Err()
		}
		return nil, nil, vmproto.Response{}, err
	}
	br := bufio.NewReader(conn)
	var resp vmproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, nil, vmproto.Response{}, ctx.Err()
		}
		return nil, nil, vmproto.Response{}, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, nil, resp, resp.Err()
	}
	return conn, br, resp, nil
}

// ContainerStream is a live bidirectional framed session returned by
// ContainerAttach or ContainerExec.
type ContainerStream struct {
	conn   net.Conn
	pid    int
	state  *vmproto.ContainerState
	stdout *framePipe
	stderr *framePipe

	writeMu     sync.Mutex
	stdinClosed bool

	done    chan struct{}
	exitMu  sync.Mutex
	exit    vmproto.ContainerExit
	exitErr error

	closeOnce sync.Once
}

func newContainerStream(conn net.Conn, br *bufio.Reader, pid int, state *vmproto.ContainerState) *ContainerStream {
	var stateCopy *vmproto.ContainerState
	if state != nil {
		cp := *state
		stateCopy = &cp
	}
	s := &ContainerStream{
		conn:   conn,
		pid:    pid,
		state:  stateCopy,
		stdout: newFramePipe(containerPipeLimit),
		stderr: newFramePipe(containerPipeLimit),
		done:   make(chan struct{}),
	}
	go s.readLoop(br)
	return s
}

// PID returns the guest PID associated with this stream (exec PID for
// ContainerExec, or container PID 1 for ContainerAttach when reported).
func (s *ContainerStream) PID() int { return s.pid }

// State returns a copy of the container state returned when attaching, or nil.
func (s *ContainerStream) State() *vmproto.ContainerState {
	if s.state == nil {
		return nil
	}
	cp := *s.state
	return &cp
}

// Stdout returns a reader yielding the process's standard output bytes.
func (s *ContainerStream) Stdout() io.Reader { return s.stdout }

// Stderr returns a reader yielding the process's standard error bytes.
func (s *ContainerStream) Stderr() io.Reader { return s.stderr }

// Stdin returns an io.WriteCloser that writes FrameStdin frames and sends
// FrameStdinEOF when closed.
func (s *ContainerStream) Stdin() io.WriteCloser {
	return streamStdinWriter{s: s}
}

type streamStdinWriter struct {
	s *ContainerStream
}

func (w streamStdinWriter) Write(p []byte) (int, error) { return w.s.WriteStdin(p) }
func (w streamStdinWriter) Close() error                { return w.s.CloseStdin() }

// WriteStdin sends p as one or more FrameStdin frames.
func (s *ContainerStream) WriteStdin(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdinClosed {
		return 0, io.ErrClosedPipe
	}
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > guestproto.MaxFrame {
			n = guestproto.MaxFrame
		}
		if err := guestproto.WriteFrame(s.conn, guestproto.FrameStdin, p[:n]); err != nil {
			return total - len(p), err
		}
		p = p[n:]
	}
	return total, nil
}

// CloseStdin sends FrameStdinEOF once to signal end-of-input to the guest.
func (s *ContainerStream) CloseStdin() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdinClosed {
		return nil
	}
	s.stdinClosed = true
	return guestproto.WriteFrame(s.conn, guestproto.FrameStdinEOF, nil)
}

// Resize sends a FrameResize frame to update the stream's PTY size.
func (s *ContainerStream) Resize(rows, cols uint16) error {
	payload, err := json.Marshal(guestproto.ContainerResizeRequest{Rows: rows, Cols: cols})
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return guestproto.WriteFrame(s.conn, guestproto.FrameResize, payload)
}

// Wait blocks until the process exits (FrameExit) or the stream disconnects.
func (s *ContainerStream) Wait() (vmproto.ContainerExit, error) {
	<-s.done
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.exit, s.exitErr
}

// Close detaches the stream, unblocks any blocked readers/Wait, and waits for
// the internal frame reader goroutine to exit. Safe to call multiple times.
func (s *ContainerStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.stdout.abort(net.ErrClosed)
		s.stderr.abort(net.ErrClosed)
		err = s.conn.Close()
		<-s.done
	})
	return err
}

func (s *ContainerStream) readLoop(br *bufio.Reader) {
	defer close(s.done)
	for {
		kind, payload, err := guestproto.ReadFrame(br)
		if err != nil {
			s.exitMu.Lock()
			if s.exitErr == nil {
				s.exitErr = err
			}
			s.exitMu.Unlock()
			s.stdout.finish(io.EOF)
			s.stderr.finish(io.EOF)
			return
		}
		switch kind {
		case guestproto.FrameStdout:
			if len(payload) > 0 && !s.stdout.push(payload) {
				s.exitMu.Lock()
				if s.exitErr == nil {
					s.exitErr = net.ErrClosed
				}
				s.exitMu.Unlock()
				return
			}
		case guestproto.FrameStderr:
			if len(payload) > 0 && !s.stderr.push(payload) {
				s.exitMu.Lock()
				if s.exitErr == nil {
					s.exitErr = net.ErrClosed
				}
				s.exitMu.Unlock()
				return
			}
		case guestproto.FrameExit:
			var ex vmproto.ContainerExit
			var exitErr error
			if len(payload) > 0 {
				if uerr := json.Unmarshal(payload, &ex); uerr != nil {
					exitErr = fmt.Errorf("vmclient: decode exit frame: %w", uerr)
				}
			}
			s.exitMu.Lock()
			s.exit = ex
			s.exitErr = exitErr
			s.exitMu.Unlock()
			s.stdout.finish(io.EOF)
			s.stderr.finish(io.EOF)
			return
		}
	}
}

// framePipe is a bounded in-memory byte stream bridging the single frame
// reader goroutine to an io.Reader consumer without deadlocking when stderr
// goes unread or Close detaches early.
type framePipe struct {
	mu       sync.Mutex
	cond     *sync.Cond
	chunks   [][]byte
	buffered int
	maxBytes int
	eof      bool
	err      error
}

func newFramePipe(maxBytes int) *framePipe {
	p := &framePipe{maxBytes: maxBytes}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *framePipe) push(data []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buffered >= p.maxBytes && !p.eof {
		p.cond.Wait()
	}
	if p.eof {
		return false
	}
	p.chunks = append(p.chunks, data)
	p.buffered += len(data)
	p.cond.Signal()
	return true
}

func (p *framePipe) finish(err error) {
	p.mu.Lock()
	if !p.eof {
		p.eof = true
		p.err = err
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *framePipe) abort(err error) {
	p.mu.Lock()
	p.eof = true
	if p.err == nil {
		p.err = err
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *framePipe) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.chunks) == 0 && !p.eof {
		p.cond.Wait()
	}
	if len(p.chunks) > 0 {
		n := copy(dst, p.chunks[0])
		if n == len(p.chunks[0]) {
			p.chunks[0] = nil
			p.chunks = p.chunks[1:]
		} else {
			p.chunks[0] = p.chunks[0][n:]
		}
		p.buffered -= n
		p.cond.Signal()
		return n, nil
	}
	if p.err != nil {
		return 0, p.err
	}
	return 0, io.EOF
}

// frameDataReader decodes a guestproto FrameData* + FrameEOF stream as an
// io.ReadCloser.
type frameDataReader struct {
	conn net.Conn
	br   *bufio.Reader

	rem  []byte
	done bool
	err  error

	closeOnce sync.Once
	closeErr  error
}

func (r *frameDataReader) Read(p []byte) (int, error) {
	for {
		if len(r.rem) > 0 {
			n := copy(p, r.rem)
			r.rem = r.rem[n:]
			return n, nil
		}
		if r.done {
			if r.err != nil {
				return 0, r.err
			}
			return 0, io.EOF
		}
		kind, payload, err := guestproto.ReadFrame(r.br)
		if err != nil {
			r.done = true
			if errors.Is(err, io.EOF) {
				r.err = io.ErrUnexpectedEOF
			} else {
				r.err = err
			}
			return 0, r.err
		}
		switch kind {
		case guestproto.FrameData:
			if len(payload) > 0 {
				r.rem = payload
			}
		case guestproto.FrameEOF:
			r.done = true
			if len(payload) > 0 {
				r.err = &vmproto.Error{Code: vmproto.CodeGuest, Message: string(payload)}
				return 0, r.err
			}
			return 0, io.EOF
		}
	}
}

func (r *frameDataReader) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.conn.Close()
	})
	return r.closeErr
}
