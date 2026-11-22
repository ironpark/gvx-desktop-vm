// Command agent is the amd64 builder VM's guest agent: PID 1's child that
// listens on vsock port guestproto.AgentPort and answers the helper
// (guestproto): hello, exec, dial, disk-mount, the services (buildkitd) and
// shutdown. It is built static for linux/arm64 by guest/mkassets and put in
// the initramfs.
//
// See docs/design/amd64-builder.md. Owner: workstream B.
package main

import (
	"log"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmassets"
)

func main() {
	log.SetPrefix("gvx-agent: ")
	log.SetFlags(0)
	ln, err := listenVsock(guestproto.AgentPort)
	if err != nil {
		log.Fatalf("vsock listen: %v", err)
	}
	defer ln.Close()

	srv := NewServer(vmassets.Version, defaultOps())
	log.Printf("listening on vsock port %d (protocol %d, version %s)", guestproto.AgentPort, guestproto.Version, vmassets.Version)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatalf("vsock accept: %v", err)
		}
		go srv.ServeConn(conn)
	}
}
