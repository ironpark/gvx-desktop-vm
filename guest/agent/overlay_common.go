package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

const (
	overlayLinkName   = "gvx0"
	overlayDNSAddr    = "127.0.0.11:53"
	overlayDNSIP      = "127.0.0.11"
	defaultOverlayMTU = 1420
	minOverlayMTU     = 576
	maxOverlayMTU     = 65500

	dnsProtoUDP byte = 0
	dnsProtoTCP byte = 1
)

// overlayNetSession represents the active guest-side overlay network (gvx0 TUN
// device + optional 127.0.0.11:53 DNS relay) attached over OpContainerNetStream.
type overlayNetSession interface {
	Serve(readFrame func() (byte, []byte, error), writeFrame func(byte, []byte) error, closeStream func())
	Update(cfg guestproto.ContainerNetConfig) error
	Close() error
}

func parseOverlayNetConfig(cfg guestproto.ContainerNetConfig) (addrs, routes []netip.Prefix, mtu int, err error) {
	for _, a := range cfg.Addresses {
		p, perr := netip.ParsePrefix(a)
		if perr != nil || !p.Addr().Is4() {
			return nil, nil, 0, fmt.Errorf("overlay address %q is not an IPv4 CIDR", a)
		}
		addrs = append(addrs, p)
	}
	for _, r := range cfg.Routes {
		p, perr := netip.ParsePrefix(r)
		if perr != nil || !p.Addr().Is4() {
			return nil, nil, 0, fmt.Errorf("overlay route %q is not an IPv4 CIDR", r)
		}
		routes = append(routes, p.Masked())
	}
	mtu = cfg.MTU
	if mtu == 0 {
		mtu = defaultOverlayMTU
	}
	if mtu < minOverlayMTU {
		return nil, nil, 0, fmt.Errorf("overlay mtu %d is below minimum %d", mtu, minOverlayMTU)
	}
	if mtu > maxOverlayMTU {
		mtu = maxOverlayMTU
	}
	return addrs, routes, mtu, nil
}

// uncoveredRoutes returns routes not already covered by the connected subnet
// routes of addrs.
func uncoveredRoutes(addrs, routes []netip.Prefix) []netip.Prefix {
	var covered []netip.Prefix
	for _, a := range addrs {
		covered = append(covered, a.Masked())
	}
	var out []netip.Prefix
	for _, r := range routes {
		r = r.Masked()
		if slices.ContainsFunc(covered, func(c netip.Prefix) bool {
			return c.Bits() <= r.Bits() && c.Contains(r.Addr())
		}) {
			continue
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// diffPrefixes returns prefixes in want not in current (toAdd) and prefixes in
// current not in want (toDel).
func diffPrefixes(current, want []netip.Prefix) (toAdd, toDel []netip.Prefix) {
	for _, p := range want {
		if !slices.Contains(current, p) && !slices.Contains(toAdd, p) {
			toAdd = append(toAdd, p)
		}
	}
	for _, p := range current {
		if !slices.Contains(want, p) && !slices.Contains(toDel, p) {
			toDel = append(toDel, p)
		}
	}
	return toAdd, toDel
}

func encodeDNSQuery(seq uint32, proto byte, query []byte) []byte {
	out := make([]byte, 5+len(query))
	binary.BigEndian.PutUint32(out[0:4], seq)
	out[4] = proto
	copy(out[5:], query)
	return out
}

func decodeDNSQuery(p []byte) (seq uint32, proto byte, query []byte, ok bool) {
	if len(p) < 6 {
		return 0, 0, nil, false
	}
	return binary.BigEndian.Uint32(p[0:4]), p[4], p[5:], true
}

func encodeDNSReply(seq uint32, reply []byte) []byte {
	out := make([]byte, 4+len(reply))
	binary.BigEndian.PutUint32(out[0:4], seq)
	copy(out[4:], reply)
	return out
}

func decodeDNSReply(p []byte) (seq uint32, reply []byte, ok bool) {
	if len(p) < 4 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint32(p[0:4]), p[4:], true
}
