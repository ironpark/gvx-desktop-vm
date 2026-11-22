// Package vmproto is what the app says to the VM helper, the separate
// process that owns the amd64 builder VM, over the helper's control socket:
// a Unix socket of mode 0600 that only the helper's own user can reach.
//
// Each connection opens with one request, a line of JSON, and gets one
// response, a line of JSON. What follows depends on the op:
//
//   - hello, status, start, stop, shutdown: nothing; the helper closes.
//   - dial: when the response is OK, the connection carries the bytes of a
//     stream in the guest (a service's socket) both ways from then on.
//   - events: the helper writes an Event line whenever the VM's state
//     changes, starting with the current one, until the client hangs up.
//
// A request names the protocol version it speaks (V). The helper answers a
// version it does not speak, and an op it does not know, with OK false and
// a Code, so that a newer app can tell an older helper apart and an older
// app can talk to a newer helper. New ops and new fields are added, never
// changed: the protocol is meant to carry amd64 containers later
// (docs/design/amd64-builder.md, "Later: amd64 containers").
//
// See docs/design/amd64-builder.md. Owner: frozen (architect). Ask the
// coordinator before changing it.
package vmproto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Version is the protocol version this package speaks.
const Version = 1

// MaxLine is the longest request or response line either side reads.
const MaxLine = 1 << 20

// The ops.
const (
	// OpHello answers Hello: who the helper is and what it speaks.
	OpHello = "hello"
	// OpStatus answers the VM's Status.
	OpStatus = "status"
	// OpStart boots the VM with Start.Spec, if it is not running, and
	// answers once the VM and every service of the spec are ready, or failed.
	// A start while one is under way waits for it. A start with a spec that
	// differs from the running VM's answers CodeBusy.
	OpStart = "start"
	// OpStop stops the services and the VM, gracefully (the guest syncs and
	// unmounts its disk) unless Stop.Force, and answers once it is stopped.
	OpStop = "stop"
	// OpDial joins the connection to a stream in the guest: Dial.Service's
	// endpoint, or Dial.Network/Dial.Addr. The VM must be running.
	OpDial = "dial"
	// OpEvents streams Event lines.
	OpEvents = "events"
	// OpShutdown stops the VM as OpStop does and makes the helper exit.
	OpShutdown = "shutdown"
)

// Codes, for a response with OK false.
const (
	CodeBadVersion  = "bad-version"   // V is one the helper does not speak
	CodeUnknownOp   = "unknown-op"    // the op is not one the helper knows
	CodeBadRequest  = "bad-request"   // the request is malformed or incomplete
	CodeNotRunning  = "not-running"   // dial or a guest op with the VM stopped
	CodeBusy        = "busy"          // start with another spec, or a stop under way
	CodeUnsupported = "unsupported"   // this Mac cannot (no Rosetta, not Apple silicon, …)
	CodeFailed      = "failed"        // the VM or a service failed; Error says how
	CodeNoService   = "no-service"    // dial names a service the guest does not have
	CodeGuest       = "guest-error"   // the guest agent refused; Error is its message
	CodeInternal    = "internal"      // anything else
	CodeShutdown    = "shutting-down" // the helper is exiting
)

// Request opens a connection.
type Request struct {
	V     int           `json:"v"`
	Op    string        `json:"op"`
	Start *StartRequest `json:"start,omitempty"`
	Stop  *StopRequest  `json:"stop,omitempty"`
	Dial  *DialRequest  `json:"dial,omitempty"`
}

// Response answers a request.
type Response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Code   string  `json:"code,omitempty"`
	Hello  *Hello  `json:"hello,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Err is the response's failure as an error, nil when OK.
func (r Response) Err() error {
	if r.OK {
		return nil
	}
	return &Error{Code: r.Code, Message: r.Error}
}

// Error is a failure the helper answered.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "vm helper: " + e.Code
	}
	return e.Message
}

// IsCode reports whether err is a helper Error with code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Hello says who the helper is.
type Hello struct {
	// Protocol is the newest version the helper speaks, MinProtocol the
	// oldest.
	Protocol    int `json:"protocol"`
	MinProtocol int `json:"minProtocol"`
	// Helper is the helper's build (the app's version when the helper is
	// the app's own executable).
	Helper string `json:"helper"`
	PID    int    `json:"pid"`
	// Features names optional abilities beyond the ops of Protocol, such as
	// "balloon"; a client checks for one before relying on it.
	Features []string `json:"features,omitempty"`
}

// State is the VM's lifecycle state.
type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	// Failed is a VM that stopped on an error, or failed to start; the
	// next start tries again.
	Failed State = "failed"
)

// Status is the VM's state and what runs in it.
type Status struct {
	State State `json:"state"`
	// Err says why the VM failed, when State is Failed.
	Err string `json:"err,omitempty"`
	// Since is when the VM entered State.
	Since time.Time `json:"since"`
	// Spec is the running (or starting) VM's spec.
	Spec     *VMSpec         `json:"spec,omitempty"`
	Services []ServiceStatus `json:"services,omitempty"`
	// Console is the file the guest's console goes to.
	Console string `json:"console,omitempty"`
	// VMPID is the pid of Virtualization.framework's VM process
	// (com.apple.Virtualization.VirtualMachine), 0 when unknown.
	VMPID int `json:"vmPid,omitempty"`
}

// ServiceStatus is one service of the guest.
type ServiceStatus struct {
	Name  string `json:"name"`
	State string `json:"state"` // "starting", "running", "stopped", "failed"
	Err   string `json:"err,omitempty"`
}

// VMSpec is the VM to boot. Paths are the host's, absolute.
type VMSpec struct {
	// Kernel is a bare arm64 Linux Image, Initrd the initramfs.
	Kernel string `json:"kernel"`
	Initrd string `json:"initrd"`
	// Assets is the asset directory, shared read-only with the guest under
	// the tag guestproto.AssetsTag: the guest runs buildkitd from it.
	Assets string `json:"assets"`
	// CPUs and MemoryMiB size the VM.
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
	// Disk is the persistent data disk, made (sparse) when missing.
	Disk *DiskSpec `json:"disk,omitempty"`
	// Rosetta shares Rosetta for Linux with the guest, which registers it
	// with binfmt_misc.
	Rosetta bool `json:"rosetta"`
	// Network gives the guest a NAT network device.
	Network bool `json:"network"`
	// Services are started in order once the guest is up and the disk is
	// mounted, by name: what a name runs is the guest's to know
	// (guestproto.ServiceBuildkitd).
	Services []string `json:"services,omitempty"`
	// CommandLine is added to the kernel command line.
	CommandLine string `json:"commandLine,omitempty"`
}

// DiskSpec is a raw disk image on the host.
type DiskSpec struct {
	Path string `json:"path"`
	// Size is the image's size when it is made; an existing image keeps
	// its own.
	Size int64 `json:"size"`
}

// StartRequest is OpStart's.
type StartRequest struct {
	Spec VMSpec `json:"spec"`
}

// StopRequest is OpStop's (and OpShutdown's, optional).
type StopRequest struct {
	// Timeout bounds a graceful stop, after which the VM is stopped by
	// force; zero is the helper's default.
	Timeout Duration `json:"timeout,omitempty"`
	// Force pulls the plug at once.
	Force bool `json:"force,omitempty"`
}

// DialRequest is OpDial's: a Service by name, or else a Network ("unix" or
// "tcp") and Addr in the guest.
type DialRequest struct {
	Service string `json:"service,omitempty"`
	Network string `json:"network,omitempty"`
	Addr    string `json:"addr,omitempty"`
}

// Event is one line of OpEvents.
type Event struct {
	Time   time.Time `json:"time"`
	Status Status    `json:"status"`
}

// Ready is the line the helper writes to its standard output once its
// control socket listens.
type Ready struct {
	Ready    bool   `json:"ready"`
	PID      int    `json:"pid"`
	Protocol int    `json:"protocol"`
	Socket   string `json:"socket"`
}

// Duration is a time.Duration that travels as milliseconds.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).Milliseconds())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var ms int64
	if err := json.Unmarshal(b, &ms); err != nil {
		return err
	}
	*d = Duration(time.Duration(ms) * time.Millisecond)
	return nil
}

// WriteLine sends v as one line of JSON.
func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) >= MaxLine {
		return fmt.Errorf("vmproto: line of %d bytes", len(b))
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadLine reads one line of JSON into v. r must be the reader every later
// read of the connection goes through, since it may buffer past the line.
func ReadLine(r *bufio.Reader, v any) error {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return err
		}
		line = append(line, chunk...)
		if len(line) >= MaxLine {
			return fmt.Errorf("vmproto: line longer than %d bytes", MaxLine)
		}
		if !isPrefix {
			break
		}
	}
	return json.Unmarshal(line, v)
}
