//go:build darwin

package vmhelper

import (
	"net"
	"syscall"
)

// waitPeerClose blocks until c's peer closes its read end (distinguishing a
// full close from a write-only CloseWrite half-close) or until done is closed.
// Returns true if the peer closed before done.
func waitPeerClose(c net.Conn, done <-chan struct{}) bool {
	sc, ok := c.(syscall.Conn)
	if !ok {
		<-done
		return false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		<-done
		return false
	}
	dupFD := -1
	var dupErr error
	if err := rc.Control(func(fd uintptr) {
		dupFD, dupErr = syscall.Dup(int(fd))
	}); err != nil || dupErr != nil || dupFD < 0 {
		<-done
		return false
	}
	defer syscall.Close(dupFD)

	kq, err := syscall.Kqueue()
	if err != nil {
		<-done
		return false
	}
	defer syscall.Close(kq)

	var pipeFDs [2]int
	if err := syscall.Pipe(pipeFDs[:]); err != nil {
		<-done
		return false
	}
	defer syscall.Close(pipeFDs[0])
	wakeDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-done:
		case <-wakeDone:
		}
		_ = syscall.Close(pipeFDs[1])
	}()
	defer func() {
		close(wakeDone)
		<-watcherDone
	}()

	changes := []syscall.Kevent_t{
		{
			Ident:  uint64(dupFD),
			Filter: syscall.EVFILT_WRITE,
			Flags:  syscall.EV_ADD | syscall.EV_CLEAR,
		},
		{
			Ident:  uint64(pipeFDs[0]),
			Filter: syscall.EVFILT_READ,
			Flags:  syscall.EV_ADD | syscall.EV_CLEAR,
		},
	}
	events := make([]syscall.Kevent_t, 2)
	first := true
	for {
		var ch []syscall.Kevent_t
		if first {
			ch = changes
			first = false
		}
		n, err := syscall.Kevent(kq, ch, events, nil)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return false
		}
		for i := range n {
			ev := events[i]
			if int(ev.Ident) == pipeFDs[0] {
				return false
			}
			if int(ev.Ident) == dupFD && ev.Flags&syscall.EV_EOF != 0 {
				return true
			}
		}
	}
}
