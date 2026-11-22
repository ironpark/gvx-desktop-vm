//go:build linux

// Command agent is what the host reaches in the vzrosetta guest: it
// listens on a virtio-vsock port and, for each connection, runs a command
// or joins the connection to a Unix socket in the guest. See the proto
// package for what is said.
//
// It is built static for linux/arm64 and put in the initramfs.
package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gvx-desktop-vm/experiments/vzrosetta/proto"
)

func main() {
	log.SetPrefix("vzagent: ")
	log.SetFlags(0)
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		log.Fatalf("vsock socket: %v", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: proto.AgentPort}); err != nil {
		log.Fatalf("vsock bind: %v", err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		log.Fatalf("vsock listen: %v", err)
	}
	log.Printf("listening on vsock port %d", proto.AgentPort)
	for {
		c, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			log.Fatalf("vsock accept: %v", err)
		}
		// Nonblocking, so that the runtime polls it and a Close wakes a
		// Read: net.FileConn knows no vsock address, os.File needs none.
		if err := unix.SetNonblock(c, true); err != nil {
			unix.Close(c)
			continue
		}
		go serve(c, os.NewFile(uintptr(c), "vsock"))
	}
}

func serve(fd int, conn *os.File) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	req, err := proto.ReadRequest(r)
	if err != nil {
		return
	}
	switch req.Op {
	case proto.OpPing:
		var u unix.Utsname
		_ = unix.Uname(&u)
		io.WriteString(conn, "pong "+unix.ByteSliceToString(u.Sysname[:])+" "+
			unix.ByteSliceToString(u.Release[:])+" "+unix.ByteSliceToString(u.Machine[:])+"\n")
	case proto.OpExec:
		run(conn, req)
	case proto.OpDial:
		dial(fd, conn, r, req.Addr)
	default:
		io.WriteString(conn, "error: unknown op "+req.Op+"\n")
	}
}

// frames turns what a command writes into frames of one kind.
type frames struct {
	mu   *sync.Mutex
	w    io.Writer
	kind byte
}

func (f frames) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := proto.WriteFrame(f.w, f.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func run(conn io.Writer, req proto.Request) {
	var mu sync.Mutex
	exit := func(code int, elapsed time.Duration, problem string) {
		mu.Lock()
		defer mu.Unlock()
		proto.WriteFrame(conn, proto.FrameExit, proto.FormatExit(code, elapsed, problem))
	}
	if len(req.Argv) == 0 {
		exit(127, 0, "no command")
		return
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Dir = req.Dir
	cmd.Stdout = frames{&mu, conn, proto.FrameStdout}
	cmd.Stderr = frames{&mu, conn, proto.FrameStderr}
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	var ee *exec.ExitError
	switch {
	case err == nil:
		exit(0, elapsed, "")
	case errors.As(err, &ee):
		exit(ee.ExitCode(), elapsed, "")
	default:
		exit(127, elapsed, err.Error())
	}
}

// dial joins the vsock connection to a Unix socket of the guest.
func dial(fd int, conn *os.File, r *bufio.Reader, addr string) {
	target, err := net.Dial("unix", addr)
	if err != nil {
		io.WriteString(conn, "error: "+err.Error()+"\n")
		return
	}
	defer target.Close()
	if _, err := io.WriteString(conn, "ok\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(target, r)
		// The host is done writing: so is the socket's client.
		target.(*net.UnixConn).CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, target)
		unix.Shutdown(fd, unix.SHUT_WR)
		done <- struct{}{}
	}()
	<-done
	<-done
}
