// Package guestproto is what the VM helper and the agent in the guest say
// to each other over virtio-vsock, and the names they share: the agent's
// port, the virtiofs tags, the kernel command line keys, the services.
//
// A connection opens with one request, a line of JSON, and gets one
// response, a line of JSON (the same framing as vmproto). What follows
// depends on the op:
//
//   - hello, disk-mount, service-start, service-stop, service-status,
//     shutdown: nothing; the agent closes.
//   - exec: frames (WriteFrame) both ways: the host sends FrameStdin and
//     FrameStdinEOF, the agent FrameStdout and FrameStderr as the command
//     writes, then one FrameExit whose payload is an Exit in JSON.
//   - dial: when the response is OK, the connection carries the bytes of
//     the guest stream both ways.
//
// VZ's vsock connections have no half close, so nothing here relies on
// one: exec's end of input is a frame, and a dial ends when either side
// closes.
//
// See docs/design/amd64-builder.md. Owner: frozen (architect). Ask the
// coordinator before changing it.
package guestproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Version is the protocol version this package speaks.
const Version = 1

// AgentPort is the vsock port the agent listens on.
const AgentPort = 1024

// The virtiofs tags the host shares and the guest mounts at /mnt/<tag>.
const (
	// AssetsTag is the asset directory (vmassets), read-only: buildkitd's
	// binaries and CA bundle.
	AssetsTag = "assets"
	// RosettaTag is Rosetta for Linux (VZLinuxRosettaDirectoryShare).
	RosettaTag = "rosetta"
)

// Kernel command line keys the guest's init reads, as key=value.
const (
	CmdlineAssets  = "gvx.assets"  // the assets tag
	CmdlineRosetta = "gvx.rosetta" // the Rosetta tag: register binfmt_misc
	CmdlineNet     = "gvx.net"     // "1": bring up eth0 with DHCP
)

// DataDevice is the persistent disk as the guest sees it (the first
// virtio-blk device), and DataMount where the agent mounts it.
const (
	DataDevice = "/dev/vda"
	DataMount  = "/var/lib/gvx"
	DataLabel  = "gvx-data"
)

// Services the agent knows by name. The guest owns what each runs.
const (
	// ServiceBuildkitd is buildkitd, its state under DataMount, listening
	// on BuildkitdSocket; its worker builds linux/amd64 (Rosetta) and
	// linux/arm64.
	ServiceBuildkitd = "buildkitd"
)

// BuildkitdSocket is buildkitd's socket in the guest.
const BuildkitdSocket = "/run/buildkit/buildkitd.sock"

// The ops.
const (
	OpHello         = "hello"
	OpExec          = "exec"
	OpDial          = "dial"
	OpDiskMount     = "disk-mount"
	OpServiceStart  = "service-start"
	OpServiceStop   = "service-stop"
	OpServiceStatus = "service-status"
	// OpShutdown stops the services, syncs and unmounts the disk, and
	// powers the guest off. The agent answers before it powers off.
	OpShutdown = "shutdown"
)

// Codes, for a response with OK false.
const (
	CodeBadVersion = "bad-version"
	CodeUnknownOp  = "unknown-op"
	CodeBadRequest = "bad-request"
	CodeNoService  = "no-service"
	CodeFailed     = "failed"
)

// Request opens a connection to the agent.
type Request struct {
	V       int             `json:"v"`
	Op      string          `json:"op"`
	Exec    *ExecRequest    `json:"exec,omitempty"`
	Dial    *DialRequest    `json:"dial,omitempty"`
	Disk    *DiskRequest    `json:"disk,omitempty"`
	Service *ServiceRequest `json:"service,omitempty"`
}

// Response answers a request.
type Response struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Code     string          `json:"code,omitempty"`
	Hello    *Hello          `json:"hello,omitempty"`
	Disk     *DiskStatus     `json:"disk,omitempty"`
	Services []ServiceStatus `json:"services,omitempty"`
}

// Hello says who the agent is and what the guest has.
type Hello struct {
	Protocol    int    `json:"protocol"`
	MinProtocol int    `json:"minProtocol"`
	Agent       string `json:"agent"`   // the asset set's version
	Kernel      string `json:"kernel"`  // uname -r
	Machine     string `json:"machine"` // uname -m
	// Rosetta is whether binfmt_misc has Rosetta registered.
	Rosetta  bool     `json:"rosetta"`
	Uptime   float64  `json:"uptime"` // seconds
	Features []string `json:"features,omitempty"`
}

// ExecRequest runs a command.
type ExecRequest struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"` // added to the agent's own
	Dir  string   `json:"dir,omitempty"`
	// Stdin says frames of input follow; without it the command reads
	// nothing.
	Stdin bool `json:"stdin,omitempty"`
}

// Exit is the payload of FrameExit.
type Exit struct {
	Code    int    `json:"code"`
	Elapsed int64  `json:"elapsedNs"`
	Error   string `json:"error,omitempty"` // why the command could not run
}

// DialRequest joins the connection to a stream of the guest: a Service's
// endpoint, or else Network ("unix", "tcp") and Addr.
type DialRequest struct {
	Service string `json:"service,omitempty"`
	Network string `json:"network,omitempty"`
	Addr    string `json:"addr,omitempty"`
}

// DiskRequest mounts DataDevice at DataMount, making an ext4 file system
// labeled DataLabel first when the device has none and Format is set.
type DiskRequest struct {
	Format bool `json:"format"`
}

// DiskStatus answers OpDiskMount.
type DiskStatus struct {
	Formatted bool   `json:"formatted"` // made a file system just now
	Size      int64  `json:"size"`
	Free      int64  `json:"free"`
	FSType    string `json:"fsType"`
}

// ServiceRequest names a service; Timeout (ms) bounds how long a start
// waits for it to be ready, or a stop for it to end.
type ServiceRequest struct {
	Name    string `json:"name"`
	Timeout int64  `json:"timeoutMs,omitempty"`
}

// ServiceStatus is one service.
type ServiceStatus struct {
	Name  string `json:"name"`
	State string `json:"state"` // "starting", "running", "stopped", "failed"
	PID   int    `json:"pid,omitempty"`
	Err   string `json:"err,omitempty"`
}

// The kinds of frame of an exec.
const (
	FrameStdout   byte = 1
	FrameStderr   byte = 2
	FrameExit     byte = 3
	FrameStdin    byte = 4
	FrameStdinEOF byte = 5
)

// MaxFrame is the largest frame payload either side sends.
const MaxFrame = 1 << 20

// WriteFrame sends one frame: its kind, its length, its bytes.
func WriteFrame(w io.Writer, kind byte, p []byte) error {
	if len(p) > MaxFrame {
		return fmt.Errorf("guestproto: frame of %d bytes", len(p))
	}
	hdr := [5]byte{kind}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	_, err := w.Write(append(hdr[:], p...))
	return err
}

// ReadFrame reads the frame WriteFrame sent.
func ReadFrame(r io.Reader) (kind byte, p []byte, err error) {
	var hdr [5]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("guestproto: frame of %d bytes", n)
	}
	p = make([]byte, n)
	_, err = io.ReadFull(r, p)
	return hdr[0], p, err
}
