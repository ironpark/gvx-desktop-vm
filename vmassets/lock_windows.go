//go:build windows

package vmassets

import (
	"context"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

var (
	inProcLockMu sync.Mutex
	inProcLocks  = map[string]*sync.Mutex{}
)

func getInProcLock(path string) *sync.Mutex {
	inProcLockMu.Lock()
	defer inProcLockMu.Unlock()
	m := inProcLocks[path]
	if m == nil {
		m = &sync.Mutex{}
		inProcLocks[path] = m
	}
	return m
}

func lockCache(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pm := getInProcLock(path)
	acquired := make(chan struct{})
	go func() {
		pm.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-ctx.Done():
		go func() {
			<-acquired
			pm.Unlock()
		}()
		return nil, ctx.Err()
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		pm.Unlock()
		return nil, err
	}

	var ol windows.Overlapped
	for {
		err = windows.LockFileEx(
			windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, 1, 0, &ol,
		)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			pm.Unlock()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}

	return func() {
		var uol windows.Overlapped
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &uol)
		_ = f.Close()
		pm.Unlock()
	}, nil
}
