//go:build darwin && arm64

package vz

import (
	"fmt"
	"net"
	"os"
	"time"
)

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock(%d:%d)", a.cid, a.port) }

// vsockConn wraps a dup'd, nonblocking virtio-vsock file descriptor as a
// net.Conn. Deadlines are handled by os.File's Go netpoller integration.
type vsockConn struct {
	f      *os.File
	local  vsockAddr
	remote vsockAddr
}

func (c *vsockConn) Read(p []byte) (int, error)         { return c.f.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error)        { return c.f.Write(p) }
func (c *vsockConn) Close() error                       { return c.f.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr               { return c.remote }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
