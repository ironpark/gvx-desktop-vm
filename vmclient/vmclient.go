// Package vmclient is the app's side of the VM helper: it starts the helper
// process and speaks vmproto to its control socket.
//
// The helper lives as long as the app wants it: its standard input is a
// pipe from the app, and when that closes (the app quit, crashed, or Close
// was called) the helper stops the VM and exits. The VM, in turn, dies with
// the helper.
//
// See docs/design/amd64-builder.md. The API (names, types, signatures) is
// frozen (architect); the bodies are workstream A's.
package vmclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

const (
	defaultStartTimeout = 10 * time.Second
	defaultCloseTimeout = 15 * time.Second
	maxStderrTailLines  = 10
)

// Launcher says how to start the helper.
type Launcher struct {
	// Path and Args are the helper's command: the app's own executable with
	// vmhelper.Arg and "serve" first, or a standalone gvx-vmhelper.
	Path string
	Args []string
	// Dir is the helper's state directory: its control socket (helper.sock,
	// so keep Dir short: a socket path may be 104 bytes at most), its log
	// and the guest's console log.
	Dir string
	// Log, when set, gets the helper's standard error, a line at a time.
	Log func(line string)
	// StartTimeout bounds the wait for the helper's Ready line; zero is
	// ten seconds.
	StartTimeout time.Duration
}

// Client is a running helper.
type Client struct {
	socket string
	cmd    *exec.Cmd
	lease  io.WriteCloser

	done chan struct{}

	mu      sync.Mutex
	closing bool
	exitErr error

	closeOnce sync.Once
	closeErr  error
}

// Launch starts the helper and waits for it to listen. A helper left over
// from an earlier run of the app (a stale socket in Dir) is never reused:
// it exits by itself when its lease closes, and Launch replaces a socket
// nobody answers on. Never a live one.
func (l Launcher) Launch(ctx context.Context) (*Client, error) {
	if strings.TrimSpace(l.Path) == "" {
		return nil, errors.New("vmclient: launcher path is required")
	}
	if strings.TrimSpace(l.Dir) == "" {
		return nil, errors.New("vmclient: launcher dir is required")
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("vmclient: create state dir: %w", err)
	}

	args := append([]string(nil), l.Args...)
	if !hasDirFlag(args) {
		args = append(args, "-dir", l.Dir)
	}

	cmd := exec.Command(l.Path, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("vmclient: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("vmclient: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("vmclient: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("vmclient: start helper: %w", err)
	}

	// Capture stderr lines for l.Log and for startup error diagnostics.
	var stderrMu sync.Mutex
	var stderrTail []string
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 4096), 256*1024)
		for sc.Scan() {
			line := sc.Text()
			stderrMu.Lock()
			if len(stderrTail) >= maxStderrTailLines {
				stderrTail = append(stderrTail[1:], line)
			} else {
				stderrTail = append(stderrTail, line)
			}
			stderrMu.Unlock()
			if l.Log != nil {
				l.Log(line)
			}
		}
	}()

	getStderrTail := func() string {
		stderrMu.Lock()
		defer stderrMu.Unlock()
		return strings.Join(stderrTail, "; ")
	}

	// Read the single Ready JSON line from stdout, then drain stdout.
	type readyRes struct {
		ready vmproto.Ready
		err   error
	}
	readyCh := make(chan readyRes, 1)
	go func() {
		br := bufio.NewReader(stdout)
		var r vmproto.Ready
		if err := vmproto.ReadLine(br, &r); err != nil {
			readyCh <- readyRes{err: err}
			return
		}
		readyCh <- readyRes{ready: r}
		_, _ = io.Copy(io.Discard, br)
	}()

	c := &Client{
		cmd:   cmd,
		lease: stdin,
		done:  make(chan struct{}),
	}

	go func() {
		waitErr := cmd.Wait()
		<-stderrDone
		c.mu.Lock()
		if !c.closing {
			if waitErr != nil {
				c.exitErr = waitErr
			} else {
				c.exitErr = errors.New("vm helper exited unexpectedly")
			}
		}
		c.mu.Unlock()
		close(c.done)
	}()

	timeout := l.StartTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	abortLaunch := func(reason error) (*Client, error) {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-c.done
		if tail := getStderrTail(); tail != "" {
			return nil, fmt.Errorf("%w (stderr: %s)", reason, tail)
		}
		return nil, reason
	}

	var res readyRes
	select {
	case res = <-readyCh:
	case <-c.done:
		return abortLaunch(errors.New("vmclient: helper exited before becoming ready"))
	case <-ctx.Done():
		return abortLaunch(fmt.Errorf("vmclient: launch canceled: %w", ctx.Err()))
	case <-timer.C:
		return abortLaunch(fmt.Errorf("vmclient: timed out after %s waiting for helper ready", timeout))
	}

	if res.err != nil {
		return abortLaunch(fmt.Errorf("vmclient: read ready line: %w", res.err))
	}
	if !res.ready.Ready || res.ready.Socket == "" {
		return abortLaunch(errors.New("vmclient: helper returned invalid ready line"))
	}
	c.socket = res.ready.Socket
	return c, nil
}

// Connect returns a Client bound to an already-running helper's control socket.
// Close on a connected client is a no-op because it does not own the helper's
// stdin lease.
func Connect(socket string) *Client {
	return &Client{socket: socket}
}

// Socket is the helper's control socket.
func (c *Client) Socket() string { return c.socket }

// Done is closed when the helper process has exited.
func (c *Client) Done() <-chan struct{} { return c.done }

// ExitErr is why the helper exited, once Done is closed: nil for an exit
// the client asked for.
func (c *Client) ExitErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

// Hello asks who the helper is.
func (c *Client) Hello(ctx context.Context) (vmproto.Hello, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{Op: vmproto.OpHello})
	if err != nil {
		return vmproto.Hello{}, err
	}
	if resp.Hello == nil {
		return vmproto.Hello{}, errors.New("vmclient: missing hello payload")
	}
	return *resp.Hello, nil
}

// Status asks for the VM's status.
func (c *Client) Status(ctx context.Context) (vmproto.Status, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{Op: vmproto.OpStatus})
	if err != nil {
		return vmproto.Status{}, err
	}
	if resp.Status == nil {
		return vmproto.Status{}, errors.New("vmclient: missing status payload")
	}
	return *resp.Status, nil
}

// Start boots the VM with spec, if it is not running, and returns once it
// and its services are ready.
func (c *Client) Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{
		Op:    vmproto.OpStart,
		Start: &vmproto.StartRequest{Spec: spec},
	})
	if err != nil {
		if resp.Status != nil {
			return *resp.Status, err
		}
		return vmproto.Status{}, err
	}
	if resp.Status == nil {
		return vmproto.Status{}, errors.New("vmclient: missing status payload")
	}
	return *resp.Status, nil
}

// Stop stops the VM; see vmproto.OpStop.
func (c *Client) Stop(ctx context.Context, req vmproto.StopRequest) (vmproto.Status, error) {
	resp, err := c.roundTrip(ctx, vmproto.Request{
		Op:   vmproto.OpStop,
		Stop: &req,
	})
	if err != nil {
		if resp.Status != nil {
			return *resp.Status, err
		}
		return vmproto.Status{}, err
	}
	if resp.Status == nil {
		return vmproto.Status{}, errors.New("vmclient: missing status payload")
	}
	return *resp.Status, nil
}

// Dial opens a stream to a guest service (vmproto.DialRequest.Service) or
// address. The returned conn is the control connection itself, past the
// response.
func (c *Client) Dial(ctx context.Context, req vmproto.DialRequest) (net.Conn, error) {
	conn, err := c.dialControl(ctx)
	if err != nil {
		return nil, err
	}
	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	if err := vmproto.WriteLine(conn, vmproto.Request{
		V:    vmproto.Version,
		Op:   vmproto.OpDial,
		Dial: &req,
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	var resp vmproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, resp.Err()
	}
	return newBufferedConn(conn, br), nil
}

// Events streams status changes to fn until ctx ends or the helper goes.
func (c *Client) Events(ctx context.Context, fn func(vmproto.Event)) error {
	conn, err := c.dialControl(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	if err := vmproto.WriteLine(conn, vmproto.Request{
		V:  vmproto.Version,
		Op: vmproto.OpEvents,
	}); err != nil {
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
	for {
		var ev vmproto.Event
		if err := vmproto.ReadLine(br, &ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if fn != nil {
			fn(ev)
		}
	}
}

// Close stops the VM (gracefully within timeout, then by force), makes the
// helper exit and waits for it.
func (c *Client) Close(timeout time.Duration) error {
	c.closeOnce.Do(func() {
		if c.lease == nil {
			return
		}
		if timeout <= 0 {
			timeout = defaultCloseTimeout
		}
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()

		// Ask the helper to shut down cleanly if it is still running.
		select {
		case <-c.done:
			_ = c.lease.Close()
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
		_, _ = c.roundTrip(ctx, vmproto.Request{
			Op:   vmproto.OpShutdown,
			Stop: &vmproto.StopRequest{Timeout: vmproto.Duration(timeout)},
		})
		cancel()

		// Closing the stdin lease guarantees exit even if the control socket
		// was unreachable.
		_ = c.lease.Close()

		timer := time.NewTimer(timeout + 5*time.Second)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			if c.cmd != nil && c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-c.done
			c.closeErr = errors.New("vmclient: timed out waiting for helper exit; killed")
		}
	})
	return c.closeErr
}

func (c *Client) dialControl(ctx context.Context) (net.Conn, error) {
	if c.socket == "" {
		return nil, errors.New("vmclient: client not connected")
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.socket)
}

func (c *Client) roundTrip(ctx context.Context, req vmproto.Request) (vmproto.Response, error) {
	conn, err := c.dialControl(ctx)
	if err != nil {
		return vmproto.Response{}, err
	}
	defer conn.Close()

	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	req.V = vmproto.Version
	if err := vmproto.WriteLine(conn, req); err != nil {
		return vmproto.Response{}, err
	}
	br := bufio.NewReader(conn)
	var resp vmproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		if ctx.Err() != nil {
			return vmproto.Response{}, ctx.Err()
		}
		return vmproto.Response{}, err
	}
	if !resp.OK {
		return resp, resp.Err()
	}
	return resp, nil
}

func watchContextClose(ctx context.Context, conn net.Conn) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

type closeWriter interface {
	CloseWrite() error
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

type bufferedCloseWriteConn struct {
	*bufferedConn
	cw closeWriter
}

func (c *bufferedCloseWriteConn) CloseWrite() error {
	return c.cw.CloseWrite()
}

func newBufferedConn(conn net.Conn, r *bufio.Reader) net.Conn {
	bc := &bufferedConn{Conn: conn, r: r}
	if cw, ok := conn.(closeWriter); ok {
		return &bufferedCloseWriteConn{bufferedConn: bc, cw: cw}
	}
	return bc
}

func hasDirFlag(args []string) bool {
	if slices.Contains(args, "-dir") || slices.Contains(args, "--dir") {
		return true
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-dir=") || strings.HasPrefix(a, "--dir=") {
			return true
		}
	}
	return false
}
