//go:build linux

package vmhelper

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func setupExecTTY(stdin io.Reader, stdout io.Writer) (rows, cols uint16, watchResize func(send func(r, c uint16)) func(), restore func()) {
	restore = func() {}

	ttyFD := -1
	if f, ok := stdin.(*os.File); ok {
		fd := int(f.Fd())
		if orig, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
			ttyFD = fd
			raw := *orig
			raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
			raw.Oflag &^= unix.OPOST
			raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
			raw.Cflag &^= unix.CSIZE | unix.PARENB
			raw.Cflag |= unix.CS8
			raw.Cc[unix.VMIN] = 1
			raw.Cc[unix.VTIME] = 0
			if unix.IoctlSetTermios(fd, unix.TCSETS, &raw) == nil {
				saved := *orig
				restore = func() {
					_ = unix.IoctlSetTermios(fd, unix.TCSETS, &saved)
				}
			}
		}
	}
	if ttyFD < 0 {
		if f, ok := stdout.(*os.File); ok {
			fd := int(f.Fd())
			if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
				ttyFD = fd
			}
		}
	}
	if ttyFD < 0 {
		return 0, 0, nil, restore
	}

	if ws, err := unix.IoctlGetWinsize(ttyFD, unix.TIOCGWINSZ); err == nil {
		rows, cols = ws.Row, ws.Col
	}

	watchResize = func(send func(r, c uint16)) func() {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGWINCH)
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-done:
					return
				case <-ch:
					if ws, err := unix.IoctlGetWinsize(ttyFD, unix.TIOCGWINSZ); err == nil && ws.Row > 0 && ws.Col > 0 {
						send(ws.Row, ws.Col)
					}
				}
			}
		}()
		return func() {
			signal.Stop(ch)
			close(done)
		}
	}

	return rows, cols, watchResize, restore
}
