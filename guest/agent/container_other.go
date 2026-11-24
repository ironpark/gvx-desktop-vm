//go:build !linux

package main

import (
	"errors"
	"io"
	"io/fs"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

func runContainerInit() {
	panic("container init is only supported on Linux")
}

type unsupportedContainerOps struct{}

func defaultContainerOps() containerOps { return unsupportedContainerOps{} }

func (unsupportedContainerOps) Start(guestproto.ContainerSpec) (guestproto.ContainerState, error) {
	return guestproto.ContainerState{}, errors.ErrUnsupported
}

func (unsupportedContainerOps) Attach() (guestproto.ContainerState, *outputHub, func([]byte) error, func() error, func(rows, cols uint16) error, error) {
	return guestproto.ContainerState{}, nil, nil, nil, nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) Signal(guestproto.ContainerSignalRequest) error {
	return errors.ErrUnsupported
}

func (unsupportedContainerOps) Resize(uint16, uint16) error {
	return errors.ErrUnsupported
}

func (unsupportedContainerOps) Exec(guestproto.ContainerExecRequest) (containerExecProc, error) {
	return nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) Top() ([]guestproto.ContainerProcess, error) {
	return nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) Stats() (guestproto.ContainerStats, error) {
	return guestproto.ContainerStats{}, errors.ErrUnsupported
}

func (unsupportedContainerOps) Pause() error   { return errors.ErrUnsupported }
func (unsupportedContainerOps) Unpause() error { return errors.ErrUnsupported }

func (unsupportedContainerOps) Stat(guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, error) {
	return guestproto.ContainerPathStat{}, errors.ErrUnsupported
}

func (unsupportedContainerOps) CopyFrom(guestproto.ContainerCopyRequest) (guestproto.ContainerPathStat, func(io.Writer) error, error) {
	return guestproto.ContainerPathStat{}, nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) CopyTo(guestproto.ContainerCopyRequest) (func(io.Reader) error, error) {
	return nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) Export(guestproto.ContainerExportRequest) (func(io.Writer) error, error) {
	return nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) NetAttach(guestproto.ContainerNetConfig) (overlayNetSession, error) {
	return nil, errors.ErrUnsupported
}

func (unsupportedContainerOps) NetUpdate(guestproto.ContainerNetConfig) error {
	return errors.ErrUnsupported
}

func (unsupportedContainerOps) Shutdown(time.Duration) error { return nil }

func isOverlayWhiteout(fs.FileInfo) bool { return false }
func isOverlayOpaqueDir(string) bool     { return false }
