package main

import (
	"bufio"
	"bytes"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

func TestOverlayHelpers(t *testing.T) {
	addrs, routes, mtu, err := parseOverlayNetConfig(guestproto.ContainerNetConfig{
		Addresses: []string{"10.89.1.2/24"},
		Routes:    []string{"10.89.0.0/16", "172.30.0.5/16"},
		MTU:       0,
	})
	if err != nil {
		t.Fatalf("parseOverlayNetConfig: %v", err)
	}
	if mtu != defaultOverlayMTU {
		t.Fatalf("mtu = %d, want %d", mtu, defaultOverlayMTU)
	}
	if len(addrs) != 1 || addrs[0].String() != "10.89.1.2/24" {
		t.Fatalf("addrs = %v", addrs)
	}
	if len(routes) != 2 || routes[1].String() != "172.30.0.0/16" {
		t.Fatalf("routes = %v", routes)
	}

	// Clamp overly large MTU and reject invalid CIDRs / small MTU.
	_, _, clamped, err := parseOverlayNetConfig(guestproto.ContainerNetConfig{MTU: 90000})
	if err != nil || clamped != maxOverlayMTU {
		t.Fatalf("clamped mtu = (%d, %v), want %d", clamped, err, maxOverlayMTU)
	}
	if _, _, _, err := parseOverlayNetConfig(guestproto.ContainerNetConfig{MTU: 100}); err == nil {
		t.Fatal("expected error for tiny MTU")
	}
	if _, _, _, err := parseOverlayNetConfig(guestproto.ContainerNetConfig{Addresses: []string{"not-a-cidr"}}); err == nil {
		t.Fatal("expected error for bad address")
	}
	if _, _, _, err := parseOverlayNetConfig(guestproto.ContainerNetConfig{Addresses: []string{"fd00::1/64"}}); err == nil {
		t.Fatal("expected error for ipv6 address")
	}

	uncov := uncoveredRoutes(
		[]netip.Prefix{netip.MustParsePrefix("10.89.1.2/24")},
		[]netip.Prefix{netip.MustParsePrefix("10.89.1.0/24"), netip.MustParsePrefix("10.89.0.0/16"), netip.MustParsePrefix("10.89.0.0/16")},
	)
	if len(uncov) != 1 || uncov[0].String() != "10.89.0.0/16" {
		t.Fatalf("uncoveredRoutes = %v, want [10.89.0.0/16]", uncov)
	}

	toAdd, toDel := diffPrefixes(
		[]netip.Prefix{netip.MustParsePrefix("10.89.1.2/24"), netip.MustParsePrefix("10.89.2.2/24")},
		[]netip.Prefix{netip.MustParsePrefix("10.89.2.2/24"), netip.MustParsePrefix("10.89.3.2/24")},
	)
	if len(toAdd) != 1 || toAdd[0].String() != "10.89.3.2/24" || len(toDel) != 1 || toDel[0].String() != "10.89.1.2/24" {
		t.Fatalf("diffPrefixes = add:%v del:%v", toAdd, toDel)
	}

	q := encodeDNSQuery(42, dnsProtoTCP, []byte{0x12, 0x34, 0x01})
	seq, proto, body, ok := decodeDNSQuery(q)
	if !ok || seq != 42 || proto != dnsProtoTCP || !bytes.Equal(body, []byte{0x12, 0x34, 0x01}) {
		t.Fatalf("decodeDNSQuery = (%d, %d, %x, %v)", seq, proto, body, ok)
	}
	r := encodeDNSReply(42, []byte{0x12, 0x34, 0x81})
	rseq, rbody, ok := decodeDNSReply(r)
	if !ok || rseq != 42 || !bytes.Equal(rbody, []byte{0x12, 0x34, 0x81}) {
		t.Fatalf("decodeDNSReply = (%d, %x, %v)", rseq, rbody, ok)
	}
}

type fakeOverlaySession struct {
	mu      sync.Mutex
	updated []guestproto.ContainerNetConfig
	gotPkt  [][]byte
	closed  bool
}

func (f *fakeOverlaySession) Serve(readFrame func() (byte, []byte, error), writeFrame func(byte, []byte) error, closeStream func()) {
	defer closeStream()
	// Echo packets back and send one DNS query expecting a reply.
	_ = writeFrame(guestproto.FrameDNSQuery, encodeDNSQuery(7, dnsProtoUDP, []byte{0xaa, 0xbb}))
	for {
		kind, payload, err := readFrame()
		if err != nil {
			return
		}
		switch kind {
		case guestproto.FrameNetPacket:
			f.mu.Lock()
			f.gotPkt = append(f.gotPkt, append([]byte(nil), payload...))
			f.mu.Unlock()
			_ = writeFrame(guestproto.FrameNetPacket, payload)
		case guestproto.FrameDNSReply:
			// done with test roundtrip
		}
	}
}

func (f *fakeOverlaySession) Update(cfg guestproto.ContainerNetConfig) error {
	f.mu.Lock()
	f.updated = append(f.updated, cfg)
	f.mu.Unlock()
	return nil
}

func (f *fakeOverlaySession) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func TestServerContainerOverlayProtocol(t *testing.T) {
	fops := newFakeContainerOps(t)
	var sess *fakeOverlaySession
	fops.netAttachFn = func(cfg guestproto.ContainerNetConfig) (overlayNetSession, error) {
		if !slices.Equal(cfg.Addresses, []string{"10.89.1.2/24"}) {
			t.Errorf("netAttach addresses = %v", cfg.Addresses)
		}
		sess = &fakeOverlaySession{}
		return sess, nil
	}
	fops.netUpdateFn = func(cfg guestproto.ContainerNetConfig) error {
		return sess.Update(cfg)
	}

	srv := NewServer("2", &fakeOps{})
	srv.ctr = fops

	// Hello must advertise FeatureOverlay.
	c1, s1 := net.Pipe()
	go srv.ServeConn(s1)
	_ = vmproto.WriteLine(c1, guestproto.Request{V: guestproto.Version, Op: guestproto.OpHello})
	var helloResp guestproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(c1), &helloResp); err != nil || !helloResp.OK {
		t.Fatalf("hello = %+v err=%v", helloResp, err)
	}
	_ = c1.Close()
	if !helloResp.Hello.HasFeature(guestproto.FeatureOverlay) {
		t.Fatalf("hello features = %v, want %q", helloResp.Hello.Features, guestproto.FeatureOverlay)
	}

	// Open OpContainerNetStream.
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeConn(server)
	}()
	_ = vmproto.WriteLine(client, guestproto.Request{
		V:  guestproto.Version,
		Op: guestproto.OpContainerNetStream,
		ContainerNet: &guestproto.ContainerNetConfig{
			Addresses: []string{"10.89.1.2/24"},
			Routes:    []string{"10.89.0.0/16"},
			MTU:       60000,
			DNS:       true,
		},
	})
	br := bufio.NewReader(client)
	var resp guestproto.Response
	if err := vmproto.ReadLine(br, &resp); err != nil || !resp.OK {
		t.Fatalf("net-stream handshake = %+v err=%v", resp, err)
	}

	// Read DNS query frame emitted by fake session.
	kind, payload, err := guestproto.ReadFrame(br)
	if err != nil || kind != guestproto.FrameDNSQuery {
		t.Fatalf("read dns query = (%d, %x, %v)", kind, payload, err)
	}
	seq, proto, q, ok := decodeDNSQuery(payload)
	if !ok || seq != 7 || proto != dnsProtoUDP || !bytes.Equal(q, []byte{0xaa, 0xbb}) {
		t.Fatalf("bad dns query payload: seq=%d proto=%d q=%x ok=%v", seq, proto, q, ok)
	}
	_ = guestproto.WriteFrame(client, guestproto.FrameDNSReply, encodeDNSReply(seq, []byte{0xaa, 0xbb, 0x01}))

	// Send IPv4 packet and verify echo back.
	pkt := make([]byte, 24)
	pkt[0] = 0x45
	if err := guestproto.WriteFrame(client, guestproto.FrameNetPacket, pkt); err != nil {
		t.Fatal(err)
	}
	kind, echoed, err := guestproto.ReadFrame(br)
	if err != nil || kind != guestproto.FrameNetPacket || !bytes.Equal(echoed, pkt) {
		t.Fatalf("echoed packet = (%d, %x, %v)", kind, echoed, err)
	}

	// OpContainerNetUpdate while stream is live.
	c2, s2 := net.Pipe()
	go srv.ServeConn(s2)
	_ = vmproto.WriteLine(c2, guestproto.Request{
		V:  guestproto.Version,
		Op: guestproto.OpContainerNetUpdate,
		ContainerNet: &guestproto.ContainerNetConfig{
			Addresses: []string{"10.89.1.2/24", "10.89.2.2/24"},
			Routes:    []string{"10.89.0.0/16"},
		},
	})
	var uresp guestproto.Response
	if err := vmproto.ReadLine(bufio.NewReader(c2), &uresp); err != nil || !uresp.OK {
		t.Fatalf("net-update = %+v err=%v", uresp, err)
	}
	_ = c2.Close()

	_ = client.Close()
	<-done

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.updated) != 1 || len(sess.updated[0].Addresses) != 2 {
		t.Fatalf("updated = %+v", sess.updated)
	}
}
