//go:build !(darwin && arm64)

package vz

import (
	"context"
	"net"
)

// Supported reports whether Virtualization.framework is available.
func Supported() bool { return false }

// RosettaAvailability says whether Rosetta for Linux can be shared.
func RosettaAvailability() Availability { return RosettaNotSupported }

// InstallRosetta asks macOS to install Rosetta and waits.
func InstallRosetta(ctx context.Context) error { return ErrUnsupported }

// VM is a virtual machine.
type VM struct{}

// New validates cfg and makes the VM on a queue of its own.
func New(cfg Config) (*VM, error) { return nil, ErrUnsupported }

// Start boots the VM and returns once VZ says it started.
func (v *VM) Start(ctx context.Context) error { return ErrUnsupported }

// Stop pulls the plug (stopWithCompletionHandler:).
func (v *VM) Stop(ctx context.Context) error { return ErrUnsupported }

// State is the VM's state now.
func (v *VM) State() State { return StateStopped }

// Done is closed when the VM has stopped, by the guest or on an error;
// Err then says which.
func (v *VM) Done() <-chan struct{} { return nil }

// Err is why the VM stopped: nil for the guest's own stop.
func (v *VM) Err() error { return nil }

// Connect opens a vsock connection to port of the guest.
func (v *VM) Connect(ctx context.Context, port uint32) (net.Conn, error) {
	return nil, ErrUnsupported
}

// Close releases the VM's resources.
func (v *VM) Close() error { return nil }
