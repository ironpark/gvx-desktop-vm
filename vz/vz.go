// Package vz drives Apple's Virtualization.framework from Go without cgo:
// through the Objective-C runtime, with github.com/ebitengine/purego and
// its objc package, so that the helper builds with CGO_ENABLED=0 and
// cross-compiles from Linux like the rest of the app.
//
// It covers what the amd64 builder needs, no more: a Linux VM booted with
// VZLinuxBootLoader, a virtio console to a file, entropy, a memory balloon,
// virtio-vsock, NAT networking, virtiofs shares (Rosetta's among them) and
// raw disk images.
//
// Threading: every message to a VZVirtualMachine goes to the VM's own
// serial dispatch queue (dispatch_sync_f, through one purego callback); the
// framework calls completion handlers and the delegate on that queue, on
// threads Go did not make, and they do nothing but hand values to Go
// channels. No run loop is needed. See docs/design/amd64-builder.md, "The
// pure-Go VZ layer".
//
// Owner: workstream A (the whole package).
package vz

import (
	"errors"
	"os"
)

// ErrUnsupported is what every call answers off macOS on Apple silicon.
var ErrUnsupported = errors.New("Virtualization.framework is not available here")

// Availability is VZLinuxRosettaDirectoryShare.availability.
type Availability int

const (
	RosettaNotSupported Availability = iota
	RosettaNotInstalled
	RosettaInstalled
)

func (a Availability) String() string {
	switch a {
	case RosettaNotInstalled:
		return "not-installed"
	case RosettaInstalled:
		return "installed"
	default:
		return "not-supported"
	}
}

// Config is a Linux VM.
type Config struct {
	Kernel, Initrd, CommandLine string
	CPUs                        int
	MemoryBytes                 uint64
	// Console gets what the guest writes to hvc0; nil drops it.
	Console *os.File
	Shares  []Share
	// Rosetta adds VZLinuxRosettaDirectoryShare under RosettaTag.
	Rosetta    bool
	RosettaTag string
	Disks      []Disk
	NAT        bool
	Balloon    bool
}

// Share is a host directory shared over virtiofs.
type Share struct {
	Tag      string
	Dir      string
	ReadOnly bool
}

// Disk is a raw disk image attached as virtio-blk.
type Disk struct {
	Path     string
	ReadOnly bool
}

// State is VZVirtualMachineState.
type State int

const (
	StateStopped State = iota
	StateRunning
	StatePaused
	StateError
	StateStarting
	StatePausing
	StateResuming
	StateStopping
	StateSaving
	StateRestoring
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateError:
		return "error"
	case StateStarting:
		return "starting"
	case StatePausing:
		return "pausing"
	case StateResuming:
		return "resuming"
	case StateStopping:
		return "stopping"
	case StateSaving:
		return "saving"
	case StateRestoring:
		return "restoring"
	default:
		return "unknown"
	}
}
