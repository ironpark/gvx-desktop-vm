//go:build !linux

package main

import (
	"errors"
	"net"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

func listenVsock(uint32) (vsockListener, error) {
	return nil, errors.ErrUnsupported
}

type unsupportedOps struct{}

func defaultOps() sysOps { return unsupportedOps{} }

func (unsupportedOps) HostInfo() hostInfo { return hostInfo{} }

func (unsupportedOps) MountDisk(bool) (guestproto.DiskStatus, error) {
	return guestproto.DiskStatus{}, errors.ErrUnsupported
}

func (unsupportedOps) UnmountDisk() error { return errors.ErrUnsupported }
func (unsupportedOps) Sync()              {}
func (unsupportedOps) PowerOff()          {}

func (unsupportedOps) StartService(string) (serviceProc, error) {
	return nil, errors.ErrUnsupported
}

func (unsupportedOps) Dial(string, string) (net.Conn, error) {
	return nil, errors.ErrUnsupported
}
