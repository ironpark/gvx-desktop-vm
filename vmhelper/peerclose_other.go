//go:build !darwin && !linux

package vmhelper

import "net"

func waitPeerClose(_ net.Conn, done <-chan struct{}) bool {
	<-done
	return false
}
