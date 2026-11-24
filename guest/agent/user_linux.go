//go:build linux

package main

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// credential returns a syscall.Credential suitable for spawning a child
// process with the resolved UID, GID, and supplementary groups.
func (u resolvedUser) credential() *syscall.Credential {
	return &syscall.Credential{
		Uid:         u.UID,
		Gid:         u.GID,
		Groups:      append([]uint32(nil), u.Groups...),
		NoSetGroups: false,
	}
}

// applyCurrentProcess drops the current process's supplementary groups,
// GID, and UID to match u. Intended for __ctr-init right before unix.Exec.
func (u resolvedUser) applyCurrentProcess() error {
	if u.UID == 0 && u.GID == 0 && len(u.Groups) == 0 {
		// Clear any inherited supplementary groups cleanly when staying root.
		_ = unix.Setgroups(nil)
		return nil
	}
	gids := make([]int, len(u.Groups))
	for i, g := range u.Groups {
		gids[i] = int(g)
	}
	if err := unix.Setgroups(gids); err != nil {
		return fmt.Errorf("setgroups(%v): %w", gids, err)
	}
	if err := unix.Setgid(int(u.GID)); err != nil {
		return fmt.Errorf("setgid(%d): %w", u.GID, err)
	}
	if err := unix.Setuid(int(u.UID)); err != nil {
		return fmt.Errorf("setuid(%d): %w", u.UID, err)
	}
	return nil
}
