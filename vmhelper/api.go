// Package vmhelper is the VM helper: the separate process that owns the
// amd64 builder VM. It makes the VM with Virtualization.framework (through
// internal/vz, pure Go), talks to the agent in the guest (guestproto), and
// serves the app on a control socket (vmproto).
//
// The helper is the app's own executable started as
//
//	<app> __vm-helper serve -dir DIR
//
// (and, for development and the engine's tests, cmd/gvx-vmhelper, the same
// code as a program of its own). It must be signed with the
// com.apple.security.virtualization entitlement.
//
// See docs/design/amd64-builder.md. This file is frozen (architect); the
// rest of the package is workstream A's.
package vmhelper

// Arg is the first argument that makes the app's executable the helper.
// main calls Main(os.Args[2:]) when os.Args[1] is Arg, before anything else.
const Arg = "__vm-helper"

// The subcommands, the first argument after Arg.
const (
	// CmdServe runs the helper: -dir DIR (the state directory: helper.sock,
	// helper.log, console.log). It writes a vmproto.Ready line on standard
	// output once it listens, and exits when standard input closes, after
	// stopping the VM.
	CmdServe = "serve"
	// CmdCheck writes a CheckResult in JSON on standard output and exits.
	CmdCheck = "check"
	// CmdInstallRosetta asks macOS to install Rosetta (it shows its own
	// dialog), waits, and writes a CheckResult.
	CmdInstallRosetta = "install-rosetta"
)

// SocketName is the control socket's name in the state directory.
const SocketName = "helper.sock"

// Rosetta availability, as VZLinuxRosettaDirectoryShare.availability says.
const (
	RosettaNotSupported = "not-supported" // not Apple silicon, or macOS before 13
	RosettaNotInstalled = "not-installed"
	RosettaInstalled    = "installed"
)

// CheckResult is what this Mac can do.
type CheckResult struct {
	// AppleSilicon and MacOS are the host's; MacOS like "27.2.1".
	AppleSilicon bool   `json:"appleSilicon"`
	MacOS        string `json:"macOS"`
	// Virtualization is whether VZ lets this process make a VM:
	// false without the entitlement, or when VZ says it is unsupported.
	Virtualization bool `json:"virtualization"`
	// Rosetta is one of the Rosetta… constants.
	Rosetta string `json:"rosetta"`
	// Err says what failed, if anything did.
	Err string `json:"err,omitempty"`
}

// OK reports whether this Mac can run the amd64 builder.
func (c CheckResult) OK() bool {
	return c.AppleSilicon && c.Virtualization && c.Rosetta == RosettaInstalled
}
