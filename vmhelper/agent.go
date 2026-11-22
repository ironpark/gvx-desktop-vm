package vmhelper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

// agentError wraps an error response from the guest agent with its guestproto code.
type agentError struct {
	code string
	msg  string
}

func (e *agentError) Error() string {
	if e.msg == "" {
		return "guest agent: " + e.code
	}
	return e.msg
}

func (e *agentError) vmProtoCode() string {
	if e.code == guestproto.CodeNoService {
		return vmproto.CodeNoService
	}
	return vmproto.CodeGuest
}

type closeWriter interface {
	CloseWrite() error
}

// bufferedConn drains any bytes buffered by a bufio.Reader (from ReadLine)
// before reading directly from the underlying net.Conn.
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

func agentRoundTrip(ctx context.Context, vm vmInstance, req guestproto.Request) (guestproto.Response, error) {
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		return guestproto.Response{}, err
	}
	defer conn.Close()

	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	req.V = guestproto.Version
	if err := vmproto.WriteLine(conn, req); err != nil {
		return guestproto.Response{}, err
	}
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		if ctx.Err() != nil {
			return guestproto.Response{}, ctx.Err()
		}
		return guestproto.Response{}, err
	}
	if !resp.OK {
		return resp, &agentError{code: resp.Code, msg: resp.Error}
	}
	return resp, nil
}

func agentHello(ctx context.Context, vm vmInstance) (guestproto.Hello, error) {
	resp, err := agentRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpHello})
	if err != nil {
		return guestproto.Hello{}, err
	}
	if resp.Hello == nil {
		return guestproto.Hello{}, errors.New("guest agent: missing hello payload")
	}
	return *resp.Hello, nil
}

func agentDiskMount(ctx context.Context, vm vmInstance, format bool) (guestproto.DiskStatus, error) {
	resp, err := agentRoundTrip(ctx, vm, guestproto.Request{
		Op:   guestproto.OpDiskMount,
		Disk: &guestproto.DiskRequest{Format: format},
	})
	if err != nil {
		return guestproto.DiskStatus{}, err
	}
	if resp.Disk == nil {
		return guestproto.DiskStatus{}, nil
	}
	return *resp.Disk, nil
}

func agentServiceStart(ctx context.Context, vm vmInstance, name string, timeout time.Duration) ([]guestproto.ServiceStatus, error) {
	resp, err := agentRoundTrip(ctx, vm, guestproto.Request{
		Op: guestproto.OpServiceStart,
		Service: &guestproto.ServiceRequest{
			Name:    name,
			Timeout: timeout.Milliseconds(),
		},
	})
	if err != nil {
		return nil, err
	}
	return resp.Services, nil
}

func agentServiceStop(ctx context.Context, vm vmInstance, name string, timeout time.Duration) ([]guestproto.ServiceStatus, error) {
	resp, err := agentRoundTrip(ctx, vm, guestproto.Request{
		Op: guestproto.OpServiceStop,
		Service: &guestproto.ServiceRequest{
			Name:    name,
			Timeout: timeout.Milliseconds(),
		},
	})
	if err != nil {
		return nil, err
	}
	return resp.Services, nil
}

func agentShutdown(ctx context.Context, vm vmInstance) error {
	_, err := agentRoundTrip(ctx, vm, guestproto.Request{Op: guestproto.OpShutdown})
	return err
}

func agentDial(ctx context.Context, vm vmInstance, req guestproto.DialRequest) (net.Conn, error) {
	conn, err := vm.Connect(ctx, guestproto.AgentPort)
	if err != nil {
		return nil, fmt.Errorf("vsock connect: %w", err)
	}
	stopWatch := watchContextClose(ctx, conn)
	defer stopWatch()

	if err := vmproto.WriteLine(conn, guestproto.Request{
		V:    guestproto.Version,
		Op:   guestproto.OpDial,
		Dial: &req,
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, &agentError{code: resp.Code, msg: resp.Error}
	}
	return newBufferedConn(conn, br), nil
}

// watchContextClose closes conn if ctx is canceled before the returned stop
// function is called, unblocking Read/Write calls.
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
