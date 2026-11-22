//go:build linux

package vmhelper

import (
	"net"
	"syscall"
)

// waitPeerClose blocks until c's peer closes its read end (EPOLLHUP/EPOLLERR,
// distinguishing full close from a write-only CloseWrite half-close) or until
// done is closed.
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

	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		<-done
		return false
	}
	defer syscall.Close(epfd)

	var pipeFDs [2]int
	if err := syscall.Pipe2(pipeFDs[:], syscall.O_CLOEXEC); err != nil {
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

	sockEv := syscall.EpollEvent{Events: 0, Fd: int32(dupFD)}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, dupFD, &sockEv); err != nil {
		<-done
		return false
	}
	pipeEv := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(pipeFDs[0])}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, pipeFDs[0], &pipeEv); err != nil {
		<-done
		return false
	}

	events := make([]syscall.EpollEvent, 2)
	for {
		n, err := syscall.EpollWait(epfd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return false
		}
		for i := range n {
			ev := events[i]
			if int(ev.Fd) == pipeFDs[0] {
				return false
			}
			if int(ev.Fd) == dupFD && ev.Events&(syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
				return true
			}
		}
	}
}
