// Package guestproto is what the VM helper and the agent in the guest say
// to each other over virtio-vsock, and the names they share: the agent's
// port, the virtiofs tags, the kernel command line keys, the services.
//
// A connection opens with one request, a line of JSON, and gets one
// response, a line of JSON (the same framing as vmproto). What follows
// depends on the op:
//
//   - hello, disk-mount, service-start, service-stop, service-status,
//     shutdown, container-start, container-signal, container-resize,
//     container-top, container-stats, container-pause, container-unpause,
//     container-stat: nothing; the agent closes.
//   - exec, container-attach, container-exec: frames (WriteFrame) both ways:
//     the host sends FrameStdin, FrameStdinEOF, and (for container ops)
//     FrameResize (JSON ContainerResizeRequest); the agent sends FrameStdout
//     and FrameStderr as the process writes, then one FrameExit whose payload
//     is an Exit in JSON.
//   - container-copy-from, container-export: when the response is OK, the
//     agent streams tar archive bytes as FrameData frames and terminates with
//     one FrameEOF frame (empty payload on clean EOF, or UTF-8 error message
//     if streaming failed mid-way).
//   - container-copy-to: when the response is OK, the host streams tar
//     archive bytes as FrameData frames followed by one empty FrameEOF frame;
//     the agent extracts and replies with one FrameEOF frame (empty payload
//     on success, or UTF-8 error message on extraction failure).
//   - dial: when the response is OK, the connection carries the bytes of
//     the guest stream both ways.
//
// VZ's vsock connections have no half close, so nothing here relies on
// one: exec/container end of input is a frame, copy/export uses FrameData +
// FrameEOF, and a dial ends when either side closes.
//
// See docs/design/amd64-builder.md and docs/design/amd64-microvm.md.
package guestproto

import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"time"
)

// Version is the protocol version this package speaks.
const Version = 1

// AgentPort is the vsock port the agent listens on.
const AgentPort = 1024

// Features advertised in Hello.Features.
const (
	// FeatureContainers indicates the agent supports the container-* ops and
	// FrameResize/FrameData/FrameEOF extensions.
	FeatureContainers = "containers"
)

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
	CmdlineShares  = "gvx.shares"  // comma-separated extra virtiofs tags (tag or tag:ro)
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
	// OpShutdown stops the services (and any running container), syncs and
	// unmounts disks/layers, and powers the guest off. The agent answers
	// before it powers off.
	OpShutdown = "shutdown"

	// Container ops (advertised via FeatureContainers).
	//
	// OpContainerStart mounts layers/volumes/binds and starts container PID 1
	// using Request.Container; answers Response.Container.
	OpContainerStart = "container-start"
	// OpContainerAttach attaches to container PID 1's buffered+live stdio.
	// Streams FrameStdout/FrameStderr/FrameExit (agent->host) and accepts
	// FrameStdin/FrameStdinEOF/FrameResize (host->agent).
	OpContainerAttach = "container-attach"
	// OpContainerSignal sends a signal using Request.ContainerSignal.
	OpContainerSignal = "container-signal"
	// OpContainerResize resizes container PID 1's PTY using Request.ContainerResize.
	OpContainerResize = "container-resize"
	// OpContainerExec runs a process in the running container using
	// Request.ContainerExec; answers Response.ExecPID then streams frames.
	OpContainerExec = "container-exec"
	// OpContainerTop lists processes in the container; answers Response.Processes.
	OpContainerTop = "container-top"
	// OpContainerStats samples cgroup v2 & network stats; answers Response.Stats.
	OpContainerStats = "container-stats"
	// OpContainerPause freezes the container cgroup.
	OpContainerPause = "container-pause"
	// OpContainerUnpause thaws the container cgroup.
	OpContainerUnpause = "container-unpause"
	// OpContainerStat stats a container path using Request.ContainerCopy;
	// answers Response.PathStat.
	OpContainerStat = "container-stat"
	// OpContainerCopyFrom streams a tar archive of Request.ContainerCopy.Path
	// as FrameData frames terminated by FrameEOF. May populate Response.PathStat.
	OpContainerCopyFrom = "container-copy-from"
	// OpContainerCopyTo extracts a tar archive sent as FrameData + FrameEOF
	// into Request.ContainerCopy.Path, replying with a final FrameEOF.
	OpContainerCopyTo = "container-copy-to"
	// OpContainerExport streams a tar archive of Request.ContainerExport
	// (full rootfs or upperOnly) as FrameData frames terminated by FrameEOF.
	OpContainerExport = "container-export"
)

// Codes, for a response with OK false.
const (
	CodeBadVersion    = "bad-version"
	CodeUnknownOp     = "unknown-op"
	CodeBadRequest    = "bad-request"
	CodeNoService     = "no-service"
	CodeFailed        = "failed"
	CodeNotFound      = "not-found"
	CodeNotRunning    = "not-running"
	CodeAlreadyPaused = "already-paused"
	CodeNotPaused     = "not-paused"
	CodePaused        = "paused"
)

// Request opens a connection to the agent.
type Request struct {
	V       int             `json:"v"`
	Op      string          `json:"op"`
	Exec    *ExecRequest    `json:"exec,omitempty"`
	Dial    *DialRequest    `json:"dial,omitempty"`
	Disk    *DiskRequest    `json:"disk,omitempty"`
	Service *ServiceRequest `json:"service,omitempty"`

	// Container op payloads (FeatureContainers).
	Container       *ContainerSpec          `json:"container,omitempty"`
	ContainerExec   *ContainerExecRequest   `json:"containerExec,omitempty"`
	ContainerSignal *ContainerSignalRequest `json:"containerSignal,omitempty"`
	ContainerResize *ContainerResizeRequest `json:"containerResize,omitempty"`
	ContainerCopy   *ContainerCopyRequest   `json:"containerCopy,omitempty"`
	ContainerExport *ContainerExportRequest `json:"containerExport,omitempty"`
}

// Response answers a request.
type Response struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Code     string          `json:"code,omitempty"`
	Hello    *Hello          `json:"hello,omitempty"`
	Disk     *DiskStatus     `json:"disk,omitempty"`
	Services []ServiceStatus `json:"services,omitempty"`

	// Container op results (FeatureContainers).
	Container *ContainerState    `json:"container,omitempty"`
	ExecPID   int                `json:"execPid,omitempty"`
	Processes []ContainerProcess `json:"processes,omitempty"`
	Stats     *ContainerStats    `json:"stats,omitempty"`
	PathStat  *ContainerPathStat `json:"pathStat,omitempty"`
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

// HasFeature reports whether Hello.Features includes name.
func (h Hello) HasFeature(name string) bool {
	return slices.Contains(h.Features, name)
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
	Error   string `json:"error,omitempty"`  // why the command could not run
	Signal  string `json:"signal,omitempty"` // signal name if terminated by signal
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

// ContainerSpec describes the single container run inside a micro-VM.
type ContainerSpec struct {
	Argv           []string         `json:"argv"`
	Env            []string         `json:"env,omitempty"`
	Workdir        string           `json:"workdir,omitempty"`
	User           string           `json:"user,omitempty"`
	Hostname       string           `json:"hostname,omitempty"`
	ExtraHosts     []string         `json:"extraHosts,omitempty"`
	TTY            bool             `json:"tty,omitempty"`
	Interactive    bool             `json:"interactive,omitempty"`
	Rows           uint16           `json:"rows,omitempty"`
	Cols           uint16           `json:"cols,omitempty"`
	Privileged     bool             `json:"privileged,omitempty"`
	ReadonlyRootfs bool             `json:"readonlyRootfs,omitempty"`
	MemoryMax      int64            `json:"memoryMax,omitempty"`
	CPUQuota       int64            `json:"cpuQuota,omitempty"`
	CPUPeriod      int64            `json:"cpuPeriod,omitempty"`
	PidsLimit      int64            `json:"pidsLimit,omitempty"`
	Layers         []LayerSource    `json:"layers"`
	Mounts         []ContainerMount `json:"mounts,omitempty"`
}

// LayerSource identifies one read-only EROFS image layer inside the guest,
// ordered bottom-to-top in ContainerSpec.Layers.
type LayerSource struct {
	Device string `json:"device,omitempty"` // fast path: /dev/vdb .. /dev/vdi
	File   string `json:"file,omitempty"`   // overflow path: /mnt/layers/<hex>.erofs via loop.ko
}

// ContainerMount is a bind, volume, or tmpfs mount inside the container rootfs.
type ContainerMount struct {
	Kind     string `json:"kind"`             // "bind", "volume", "tmpfs"
	Source   string `json:"source,omitempty"` // virtiofs path, block dev, or ""
	Target   string `json:"target"`           // container path
	ReadOnly bool   `json:"readOnly,omitempty"`
	Options  string `json:"options,omitempty"` // tmpfs options (e.g. size=,mode=)
}

// ContainerExecRequest starts a process inside the running container.
type ContainerExecRequest struct {
	Argv        []string `json:"argv"`
	Env         []string `json:"env,omitempty"`
	Workdir     string   `json:"workdir,omitempty"`
	User        string   `json:"user,omitempty"`
	TTY         bool     `json:"tty,omitempty"`
	Interactive bool     `json:"interactive,omitempty"`
	Rows        uint16   `json:"rows,omitempty"`
	Cols        uint16   `json:"cols,omitempty"`
	Privileged  bool     `json:"privileged,omitempty"`
}

// ContainerSignalRequest delivers a signal inside the container.
type ContainerSignalRequest struct {
	Signal string `json:"signal"`        // signal name or number; "" defaults to SIGKILL
	PID    int    `json:"pid,omitempty"` // guest PID; 0 targets container PID 1
	All    bool   `json:"all,omitempty"` // signal all processes in the container
}

// ContainerResizeRequest sets a PTY window size (used for OpContainerResize
// and as the JSON payload of FrameResize).
type ContainerResizeRequest struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// ContainerCopyRequest configures OpContainerStat, OpContainerCopyFrom, and
// OpContainerCopyTo.
type ContainerCopyRequest struct {
	Path           string `json:"path"`
	Archive        bool   `json:"archive,omitempty"`
	FollowLink     bool   `json:"followLink,omitempty"`
	NoOverwriteDir bool   `json:"noOverwriteDir,omitempty"`
	User           string `json:"user,omitempty"`
}

// ContainerExportRequest configures OpContainerExport.
type ContainerExportRequest struct {
	Path      string `json:"path,omitempty"`      // "" exports "/"
	UpperOnly bool   `json:"upperOnly,omitempty"` // export only /var/lib/gvx/upper
}

// ContainerState describes the container's runtime status.
type ContainerState struct {
	Running  bool   `json:"running"`
	Paused   bool   `json:"paused,omitempty"`
	PID      int    `json:"pid,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ContainerProcess is one process reported by OpContainerTop.
type ContainerProcess struct {
	PID      int32    `json:"pid"`
	PPID     int32    `json:"ppid"`
	UID      uint32   `json:"uid"`
	GID      uint32   `json:"gid"`
	State    string   `json:"state"`
	Threads  int      `json:"threads"`
	CPUNanos int64    `json:"cpuNs"`
	RSSBytes uint64   `json:"rssBytes"`
	Comm     string   `json:"comm"`
	Cmdline  []string `json:"cmdline,omitempty"`
}

// ContainerNetStats holds per-interface network counters for ContainerStats.
type ContainerNetStats struct {
	Interface string `json:"interface"`
	RxBytes   uint64 `json:"rxBytes"`
	TxBytes   uint64 `json:"txBytes"`
	RxPackets uint64 `json:"rxPackets"`
	TxPackets uint64 `json:"txPackets"`
}

// ContainerStats is a resource usage sample reported by OpContainerStats.
type ContainerStats struct {
	Timestamp        time.Time           `json:"timestamp"`
	CPUUserNanos     int64               `json:"cpuUserNs"`
	CPUSystemNanos   int64               `json:"cpuSysNs"`
	CPUPeriods       uint64              `json:"cpuPeriods,omitempty"`
	CPUThrottled     uint64              `json:"cpuThrottled,omitempty"`
	CPUThrottledNs   int64               `json:"cpuThrottledNs,omitempty"`
	MemoryBytes      uint64              `json:"memoryBytes"`
	MemoryAnonBytes  uint64              `json:"memoryAnonBytes,omitempty"`
	MemoryFileBytes  uint64              `json:"memoryFileBytes,omitempty"`
	MemoryTmpfsBytes uint64              `json:"memoryTmpfsBytes,omitempty"`
	MemoryLimitBytes uint64              `json:"memoryLimitBytes"`
	Network          []ContainerNetStats `json:"network,omitempty"`
	PIDs             int                 `json:"pids"`
	PIDsLimit        int64               `json:"pidsLimit,omitempty"`
}

// ContainerPathStat describes a path inside the container rootfs.
type ContainerPathStat struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"` // Go os.FileMode bits
	Mtime      time.Time `json:"mtime,omitempty"`
	LinkTarget string    `json:"linkTarget,omitempty"`
}

// The kinds of frame of an exec or container stream.
const (
	FrameStdout   byte = 1
	FrameStderr   byte = 2
	FrameExit     byte = 3
	FrameStdin    byte = 4
	FrameStdinEOF byte = 5
	FrameResize   byte = 6 // payload: JSON ContainerResizeRequest
	FrameData     byte = 7 // payload: raw stream chunk (copy-from, copy-to, export)
	FrameEOF      byte = 8 // payload: empty on clean EOF, or UTF-8 error message
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
