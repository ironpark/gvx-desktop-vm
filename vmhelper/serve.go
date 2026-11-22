package vmhelper

// Owner: workstream A.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop-vm/vz"
)

// serve runs the control server until standard input closes or shutdown is
// requested.
func serve(args []string) error {
	fs := flag.NewFlagSet(CmdServe, flag.ContinueOnError)
	dir := fs.String("dir", "", "state directory for helper.sock and console.log")
	version := fs.String("version", "dev", "helper version reported in hello")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("serve requires -dir")
	}

	sockPath := filepath.Join(*dir, SocketName)
	ln, err := listenUnix(sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()

	srv := newServer(serverOptions{
		dir:           *dir,
		helperVersion: *version,
	})

	// Announce readiness before watching stdin so the launcher sees Ready
	// immediately once the socket accepts.
	ready := vmproto.Ready{
		Ready:    true,
		PID:      os.Getpid(),
		Protocol: vmproto.Version,
		Socket:   sockPath,
	}
	if err := vmproto.WriteLine(os.Stdout, ready); err != nil {
		return fmt.Errorf("write ready line: %w", err)
	}

	// Lease: EOF on stdin (parent quit, crashed, or Close) or SIGINT/SIGTERM
	// triggers graceful VM stop and clean exit.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	stdinEOF := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinEOF)
	}()

	go func() {
		select {
		case <-stdinEOF:
		case <-sigCh:
		case <-srv.srvCtx.Done():
			return
		}
		srv.Shutdown(vmproto.StopRequest{Timeout: vmproto.Duration(defaultStopTimeout)})
	}()

	return srv.Serve(ln)
}

// check reports what this Mac can do, installing Rosetta first when install is
// set.
func check(install bool) CheckResult {
	res := CheckResult{
		AppleSilicon:   runtime.GOOS == "darwin" && runtime.GOARCH == "arm64",
		MacOS:          hostMacOSVersion(),
		Virtualization: vz.Supported(),
		Rosetta:        rosettaString(vz.RosettaAvailability()),
	}
	if install && res.Rosetta == RosettaNotInstalled {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := vz.InstallRosetta(ctx)
		cancel()
		if err != nil {
			res.Err = err.Error()
		}
		res.Rosetta = rosettaString(vz.RosettaAvailability())
	}
	if res.Err == "" && res.AppleSilicon && !res.Virtualization {
		res.Err = "Virtualization.framework is not available or the virtualization entitlement is missing"
	}
	return res
}

func rosettaString(a vz.Availability) string {
	switch a {
	case vz.RosettaInstalled:
		return RosettaInstalled
	case vz.RosettaNotInstalled:
		return RosettaNotInstalled
	default:
		return RosettaNotSupported
	}
}
