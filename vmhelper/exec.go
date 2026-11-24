package vmhelper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runExec(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return execWithIO(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

func execWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(CmdExec, flag.ContinueOnError)
	fs.SetOutput(stderr)

	socketPath := fs.String("socket", "", "path to the helper control socket")
	interactive := fs.Bool("i", false, "forward standard input")
	tty := fs.Bool("t", false, "allocate a PTY in the container")
	user := fs.String("u", "", "user[:group] to run as")
	workdir := fs.String("w", "", "working directory inside the container")
	privileged := fs.Bool("privileged", false, "run with full container privileges")
	rowsFlag := fs.Uint("rows", 0, "initial terminal rows (0 = detect)")
	colsFlag := fs.Uint("cols", 0, "initial terminal cols (0 = detect)")
	var envVars stringSliceFlag
	fs.Var(&envVars, "e", "environment variable KEY=VALUE (repeatable)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *socketPath == "" {
		fmt.Fprintln(stderr, "vm helper exec: -socket is required")
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "vm helper exec: command argv is required")
		return 2
	}

	var rows, cols uint16
	var watchResize func(send func(r, c uint16)) func()
	restoreTTY := func() {}
	if *tty {
		detRows, detCols, watcher, restore := setupExecTTY(stdin, stdout)
		restoreTTY = restore
		watchResize = watcher
		rows, cols = detRows, detCols
	}
	defer restoreTTY()

	if *rowsFlag > 0 && *rowsFlag <= 0xffff {
		rows = uint16(*rowsFlag)
	}
	if *colsFlag > 0 && *colsFlag <= 0xffff {
		cols = uint16(*colsFlag)
	}

	var dialer net.Dialer
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := dialer.DialContext(dialCtx, "unix", *socketPath)
	dialCancel()
	if err != nil {
		fmt.Fprintf(stderr, "vm helper exec: connect %s: %v\n", *socketPath, err)
		return 1
	}
	defer conn.Close()

	stopCtxWatch := watchContextClose(ctx, conn)
	defer stopCtxWatch()

	req := vmproto.Request{
		V:  vmproto.Version,
		Op: vmproto.OpContainerExec,
		ContainerExec: &vmproto.ContainerExecRequest{
			Argv:        argv,
			Env:         []string(envVars),
			Workdir:     *workdir,
			User:        *user,
			TTY:         *tty,
			Interactive: *interactive,
			Rows:        rows,
			Cols:        cols,
			Privileged:  *privileged,
		},
	}
	if err := vmproto.WriteLine(conn, req); err != nil {
		fmt.Fprintf(stderr, "vm helper exec: write request: %v\n", err)
		return 1
	}

	br := bufio.NewReader(conn)
	var resp vmproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil {
		fmt.Fprintf(stderr, "vm helper exec: read response: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "vm helper exec: %v\n", resp.Err())
		return 1
	}

	var writeMu sync.Mutex
	writeFrame := func(kind byte, p []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return guestproto.WriteFrame(conn, kind, p)
	}

	if watchResize != nil {
		stopResize := watchResize(func(r, c uint16) {
			payload, err := json.Marshal(guestproto.ContainerResizeRequest{Rows: r, Cols: c})
			if err != nil {
				return
			}
			_ = writeFrame(guestproto.FrameResize, payload)
		})
		defer stopResize()
	}

	if *interactive && stdin != nil {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, rerr := stdin.Read(buf)
				if n > 0 {
					if werr := writeFrame(guestproto.FrameStdin, buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					_ = writeFrame(guestproto.FrameStdinEOF, nil)
					return
				}
			}
		}()
	}

	for {
		kind, payload, err := guestproto.ReadFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return 1
			}
			fmt.Fprintf(stderr, "vm helper exec: read stream: %v\n", err)
			return 1
		}
		switch kind {
		case guestproto.FrameStdout:
			if len(payload) > 0 && stdout != nil {
				_, _ = stdout.Write(payload)
			}
		case guestproto.FrameStderr:
			if len(payload) > 0 && stderr != nil {
				_, _ = stderr.Write(payload)
			}
		case guestproto.FrameExit:
			var ex guestproto.Exit
			if len(payload) > 0 {
				if uerr := json.Unmarshal(payload, &ex); uerr != nil {
					fmt.Fprintf(stderr, "vm helper exec: decode exit: %v\n", uerr)
					return 1
				}
			}
			if ex.Error != "" {
				fmt.Fprintf(stderr, "vm helper exec: %s\n", ex.Error)
				if ex.Code == 0 {
					return 1
				}
			}
			return ex.Code
		}
	}
}
