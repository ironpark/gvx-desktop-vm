//go:build !windows

package vmassets

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
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

	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			pm.Unlock()
			return nil, err
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
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		pm.Unlock()
	}, nil
}
