//go:build darwin && arm64 && cgo

// Command vzrosetta is an experiment: a small Linux VM made with
// Virtualization.framework, with Rosetta for Linux, to see what running
// amd64 containers and builds beside the gvx sandboxes would take. It is
// not part of the app. README.md has the steps and what was measured.
//
//	vzrosetta run   -kernel Image -initrd initramfs.gz [-rosetta] [-share tag=dir] …
//	vzrosetta ping  -sock agent.sock
//	vzrosetta exec  -sock agent.sock -- command…
//
// The binary must be signed with the com.apple.security.virtualization
// entitlement: see build.sh.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/ironpark/gvx-desktop-vm/experiments/vzrosetta/proto"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: vzrosetta run|ping|exec …")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runVM(os.Args[2:])
	case "ping":
		err = ping(os.Args[2:])
	case "exec":
		err = execIn(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vzrosetta:", err)
		os.Exit(1)
	}
}

// ping asks the agent who it is, through the socket run forwards to it,
// and tells how long the answer took.
func ping(args []string) error {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	sock := fs.String("sock", "/tmp/gvxd-vz/agent.sock", "the socket run forwards to the agent")
	fs.Parse(args)
	start := time.Now()
	conn, err := net.Dial("unix", *sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := proto.WriteRequest(conn, proto.Request{Op: proto.OpPing}); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("no answer: %w", err)
	}
	fmt.Printf("%s(round trip %s)\n", line, time.Since(start).Round(time.Microsecond))
	return nil
}

// execIn runs a command in the guest and exits as it did.
func execIn(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	sock := fs.String("sock", "/tmp/gvxd-vz/agent.sock", "the socket run forwards to the agent")
	timing := fs.Bool("time", false, "tell on stderr how long the command ran, as the agent saw it")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("exec: no command")
	}
	conn, err := net.Dial("unix", *sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := proto.WriteRequest(conn, proto.Request{Op: proto.OpExec, Argv: fs.Args()}); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	for {
		kind, p, err := proto.ReadFrame(r)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("the agent hung up: %w", err)
		}
		switch kind {
		case proto.FrameStdout:
			os.Stdout.Write(p)
		case proto.FrameStderr:
			os.Stderr.Write(p)
		case proto.FrameExit:
			code, elapsed, problem, err := proto.ParseExit(p)
			if err != nil {
				return err
			}
			if problem != "" {
				fmt.Fprintln(os.Stderr, "vzrosetta: guest:", problem)
			}
			if *timing {
				fmt.Fprintf(os.Stderr, "[guest elapsed %.3fs]\n", elapsed.Seconds())
			}
			os.Exit(code)
		}
	}
}
