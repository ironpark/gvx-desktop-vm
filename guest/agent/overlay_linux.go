//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
)

const (
	maxInFlightDNS = 64
	dnsExchangeTTL = 6 * time.Second
)

type linuxOverlaySession struct {
	tunFile *os.File
	ifIndex uint32
	udpPC   net.PacketConn
	tcpLN   net.Listener

	applyMu sync.Mutex

	closed      chan struct{}
	closeOnce   sync.Once
	streamMu    sync.Mutex
	closeStream func()

	nextSeq   atomic.Uint32
	dnsSem    chan struct{}
	pendingMu sync.Mutex
	pending   map[uint32]chan []byte

	onClose func(*linuxOverlaySession)
}

func startLinuxOverlaySession(cfg guestproto.ContainerNetConfig, onClose func(*linuxOverlaySession)) (*linuxOverlaySession, error) {
	addrs, routes, mtu, err := parseOverlayNetConfig(cfg)
	if err != nil {
		return nil, errWithCode(guestproto.CodeBadRequest, err)
	}

	tunFile, err := openOverlayTun(overlayLinkName)
	if err != nil {
		return nil, fmt.Errorf("open overlay tun %s: %w", overlayLinkName, err)
	}

	cleanup := func() {
		_ = tunFile.Close()
	}

	iface, err := net.InterfaceByName(overlayLinkName)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("find %s: %w", overlayLinkName, err)
	}
	idx := uint32(iface.Index)

	if err := reconcileOverlayLink(idx, addrs, routes, mtu); err != nil {
		cleanup()
		return nil, err
	}

	var (
		udpPC net.PacketConn
		tcpLN net.Listener
	)
	if cfg.DNS {
		udpPC, err = net.ListenPacket("udp4", overlayDNSAddr)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("listen overlay dns udp %s: %w", overlayDNSAddr, err)
		}
		tcpLN, err = net.Listen("tcp4", overlayDNSAddr)
		if err != nil {
			_ = udpPC.Close()
			cleanup()
			return nil, fmt.Errorf("listen overlay dns tcp %s: %w", overlayDNSAddr, err)
		}
	}

	return &linuxOverlaySession{
		tunFile: tunFile,
		ifIndex: idx,
		udpPC:   udpPC,
		tcpLN:   tcpLN,
		closed:  make(chan struct{}),
		dnsSem:  make(chan struct{}, maxInFlightDNS),
		pending: make(map[uint32]chan []byte),
		onClose: onClose,
	}, nil
}

func (s *linuxOverlaySession) Update(cfg guestproto.ContainerNetConfig) error {
	addrs, routes, mtu, err := parseOverlayNetConfig(cfg)
	if err != nil {
		return errWithCode(guestproto.CodeBadRequest, err)
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	select {
	case <-s.closed:
		return errWithCode(guestproto.CodeNotRunning, errors.New("overlay session closed"))
	default:
	}
	return reconcileOverlayLink(s.ifIndex, addrs, routes, mtu)
}

func (s *linuxOverlaySession) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.udpPC != nil {
			_ = s.udpPC.Close()
		}
		if s.tcpLN != nil {
			_ = s.tcpLN.Close()
		}
		if s.tunFile != nil {
			_ = s.tunFile.Close()
		}
		s.streamMu.Lock()
		cs := s.closeStream
		s.streamMu.Unlock()
		if cs != nil {
			cs()
		}
		s.pendingMu.Lock()
		for seq, ch := range s.pending {
			close(ch)
			delete(s.pending, seq)
		}
		s.pendingMu.Unlock()
		if s.onClose != nil {
			s.onClose(s)
		}
	})
	return nil
}

func (s *linuxOverlaySession) Serve(readFrame func() (byte, []byte, error), writeFrame func(byte, []byte) error, closeStream func()) {
	s.streamMu.Lock()
	s.closeStream = closeStream
	select {
	case <-s.closed:
		s.streamMu.Unlock()
		if closeStream != nil {
			closeStream()
		}
		return
	default:
	}
	s.streamMu.Unlock()
	defer s.Close()

	var wg sync.WaitGroup

	// 1. TUN -> host (FrameNetPacket).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.Close()
		buf := make([]byte, maxOverlayMTU+128)
		for {
			n, err := s.tunFile.Read(buf)
			if err != nil {
				return
			}
			if n < 20 || buf[0]>>4 != 4 {
				continue
			}
			if err := writeFrame(guestproto.FrameNetPacket, buf[:n]); err != nil {
				return
			}
		}
	}()

	// 2. Guest 127.0.0.11:53 UDP -> host (FrameDNSQuery).
	if s.udpPC != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveDNSUDP(writeFrame)
		}()
	}

	// 3. Guest 127.0.0.11:53 TCP -> host (FrameDNSQuery).
	if s.tcpLN != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveDNSTCP(writeFrame)
		}()
	}

	// 4. Host -> guest frames (FrameNetPacket -> TUN, FrameDNSReply -> DNS waiters).
	for {
		kind, payload, err := readFrame()
		if err != nil {
			break
		}
		switch kind {
		case guestproto.FrameNetPacket:
			if len(payload) >= 20 && payload[0]>>4 == 4 {
				_, _ = s.tunFile.Write(payload)
			}
		case guestproto.FrameDNSReply:
			seq, reply, ok := decodeDNSReply(payload)
			if !ok {
				continue
			}
			s.pendingMu.Lock()
			ch := s.pending[seq]
			delete(s.pending, seq)
			s.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- append([]byte(nil), reply...):
				default:
				}
			}
		}
	}

	s.Close()
	wg.Wait()
}

func (s *linuxOverlaySession) exchangeDNS(proto byte, q []byte, writeFrame func(byte, []byte) error) []byte {
	if len(q) == 0 || len(q) > 65535 {
		return nil
	}
	select {
	case s.dnsSem <- struct{}{}:
		defer func() { <-s.dnsSem }()
	case <-s.closed:
		return nil
	}

	seq := s.nextSeq.Add(1)
	ch := make(chan []byte, 1)

	s.pendingMu.Lock()
	select {
	case <-s.closed:
		s.pendingMu.Unlock()
		return nil
	default:
		s.pending[seq] = ch
	}
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, seq)
		s.pendingMu.Unlock()
	}()

	if err := writeFrame(guestproto.FrameDNSQuery, encodeDNSQuery(seq, proto, q)); err != nil {
		return nil
	}

	timer := time.NewTimer(dnsExchangeTTL)
	defer timer.Stop()
	select {
	case reply, ok := <-ch:
		if !ok {
			return nil
		}
		return reply
	case <-s.closed:
		return nil
	case <-timer.C:
		return nil
	}
}

func (s *linuxOverlaySession) serveDNSUDP(writeFrame func(byte, []byte) error) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := s.udpPC.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func(query []byte, peer net.Addr) {
			if reply := s.exchangeDNS(dnsProtoUDP, query, writeFrame); len(reply) > 0 {
				_, _ = s.udpPC.WriteTo(reply, peer)
			}
		}(q, addr)
	}
}

func (s *linuxOverlaySession) serveDNSTCP(writeFrame func(byte, []byte) error) {
	for {
		conn, err := s.tcpLN.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				var hdr [2]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				n := binary.BigEndian.Uint16(hdr[:])
				if n == 0 {
					return
				}
				q := make([]byte, n)
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				reply := s.exchangeDNS(dnsProtoTCP, q, writeFrame)
				if len(reply) == 0 || len(reply) > 65535 {
					return
				}
				binary.BigEndian.PutUint16(hdr[:], uint16(len(reply)))
				out := make([]byte, 2+len(reply))
				copy(out[:2], hdr[:])
				copy(out[2:], reply)
				if _, err := c.Write(out); err != nil {
					return
				}
			}
		}(conn)
	}
}

// ---- /dev/net/tun + rtnetlink helpers ----

func openOverlayTun(name string) (*os.File, error) {
	if _, err := os.Stat("/dev/net/tun"); errors.Is(err, os.ErrNotExist) {
		_ = os.MkdirAll("/dev/net", 0o755)
		if merr := unix.Mknod("/dev/net/tun", unix.S_IFCHR|0o666, int(unix.Mkdev(10, 200))); merr != nil && !errors.Is(merr, unix.EEXIST) {
			return nil, fmt.Errorf("mknod /dev/net/tun: %w", merr)
		}
		_ = unix.Chmod("/dev/net/tun", 0o666)
	}
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	var ifr struct {
		name  [unix.IFNAMSIZ]byte
		flags uint16
		pad   [22]byte
	}
	copy(ifr.name[:], name)
	ifr.flags = unix.IFF_TUN | unix.IFF_NO_PI
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %s: %w", name, errno)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

func reconcileOverlayLink(index uint32, addrs, routes []netip.Prefix, mtu int) error {
	iface, err := net.InterfaceByIndex(int(index))
	if err != nil {
		return fmt.Errorf("lookup link %d: %w", index, err)
	}
	rawAddrs, err := iface.Addrs()
	if err != nil {
		return fmt.Errorf("read %s addresses: %w", overlayLinkName, err)
	}
	var current []netip.Prefix
	for _, a := range rawAddrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		current = append(current, netip.PrefixFrom(netip.AddrFrom4([4]byte(ip4)), ones))
	}

	rt, err := nlDial(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer rt.close()

	if mtu > 0 {
		if err := nlSetLinkMTU(rt, index, uint32(mtu)); err != nil {
			return err
		}
	}

	curRoutes, err := nlListBootRoutes(rt, index)
	if err != nil {
		return err
	}

	toAdd, toDel := diffPrefixes(current, addrs)
	for _, p := range toDel {
		if err := nlDelAddr(rt, index, p); err != nil {
			return err
		}
	}
	for _, p := range toAdd {
		if err := nlAddAddr(rt, index, p); err != nil {
			return err
		}
	}

	if len(addrs) == 0 {
		for _, p := range curRoutes {
			if err := nlDelRoute(rt, p, index); err != nil {
				return err
			}
		}
		return nlSetLinkUpDown(rt, index, false)
	}

	if err := nlSetLinkUpDown(rt, index, true); err != nil {
		return err
	}
	wantRoutes := uncoveredRoutes(addrs, routes)
	addRoutes, delRoutes := diffPrefixes(curRoutes, wantRoutes)
	for _, p := range delRoutes {
		if err := nlDelRoute(rt, p, index); err != nil {
			return err
		}
	}
	for _, p := range addRoutes {
		if err := nlAddRoute(rt, p, index); err != nil {
			return err
		}
	}
	return nil
}

type nlSock struct {
	fd  int
	seq uint32
}

func nlDial(proto int) (*nlSock, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	return &nlSock{fd: fd}, nil
}

func (s *nlSock) close() { _ = unix.Close(s.fd) }

func (s *nlSock) request(typ, flags uint16, payload []byte) ([][]byte, error) {
	s.seq++
	msg := make([]byte, unix.NLMSG_HDRLEN, unix.NLMSG_HDRLEN+len(payload))
	binary.NativeEndian.PutUint32(msg[0:], uint32(unix.NLMSG_HDRLEN+len(payload)))
	binary.NativeEndian.PutUint16(msg[4:], typ)
	binary.NativeEndian.PutUint16(msg[6:], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:], s.seq)
	msg = append(msg, payload...)
	if err := unix.Sendto(s.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	var out [][]byte
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			return nil, err
		}
		data := buf[:n]
		for len(data) >= unix.NLMSG_HDRLEN {
			l := int(binary.NativeEndian.Uint32(data[0:]))
			if l < unix.NLMSG_HDRLEN || l > len(data) {
				return nil, errors.New("netlink: short message")
			}
			mtype := binary.NativeEndian.Uint16(data[4:])
			seq := binary.NativeEndian.Uint32(data[8:])
			body := data[unix.NLMSG_HDRLEN:l]
			data = data[min(nlAlign(l), len(data)):]
			if seq != s.seq {
				continue
			}
			switch mtype {
			case unix.NLMSG_ERROR:
				if len(body) < 4 {
					return nil, errors.New("netlink: short error")
				}
				if code := int32(binary.NativeEndian.Uint32(body)); code != 0 {
					return nil, syscall.Errno(-code)
				}
				return out, nil
			case unix.NLMSG_DONE:
				return out, nil
			default:
				out = append(out, append([]byte(nil), body...))
			}
		}
	}
}

func (s *nlSock) do(typ, flags uint16, payload []byte) error {
	_, err := s.request(typ, flags, payload)
	return err
}

func nlAlign(n int) int { return (n + 3) &^ 3 }

type nlAttrs []byte

func (b *nlAttrs) bytes(typ uint16, val []byte) {
	var h [4]byte
	binary.NativeEndian.PutUint16(h[0:], uint16(4+len(val)))
	binary.NativeEndian.PutUint16(h[2:], typ)
	*b = append(*b, h[:]...)
	*b = append(*b, val...)
	for len(*b)%4 != 0 {
		*b = append(*b, 0)
	}
}

func (b *nlAttrs) u32(typ uint16, v uint32) {
	var val [4]byte
	binary.NativeEndian.PutUint32(val[:], v)
	b.bytes(typ, val[:])
}

func nlParseAttrs(data []byte, fn func(typ uint16, val []byte)) {
	for len(data) >= 4 {
		l := int(binary.NativeEndian.Uint16(data[0:]))
		typ := binary.NativeEndian.Uint16(data[2:]) &^ (unix.NLA_F_NESTED | unix.NLA_F_NET_BYTEORDER)
		if l < 4 || l > len(data) {
			return
		}
		fn(typ, data[4:l])
		data = data[min(nlAlign(l), len(data)):]
	}
}

func nlSetLinkMTU(rt *nlSock, index, mtu uint32) error {
	ifi := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(ifi[4:], index)
	var a nlAttrs
	a.u32(unix.IFLA_MTU, mtu)
	if err := rt.do(unix.RTM_NEWLINK, 0, append(ifi, a...)); err != nil {
		return fmt.Errorf("set link %s mtu %d: %w", overlayLinkName, mtu, err)
	}
	return nil
}

func nlSetLinkUpDown(rt *nlSock, index uint32, up bool) error {
	ifi := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(ifi[4:], index)
	if up {
		binary.NativeEndian.PutUint32(ifi[8:], unix.IFF_UP)
	}
	binary.NativeEndian.PutUint32(ifi[12:], unix.IFF_UP)
	if err := rt.do(unix.RTM_NEWLINK, 0, ifi); err != nil {
		return fmt.Errorf("set link %s up=%v: %w", overlayLinkName, up, err)
	}
	return nil
}

func nlAddAddr(rt *nlSock, index uint32, p netip.Prefix) error {
	msg := make([]byte, unix.SizeofIfAddrmsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())
	msg[3] = unix.RT_SCOPE_UNIVERSE
	binary.NativeEndian.PutUint32(msg[4:], index)
	var a nlAttrs
	ip := p.Addr().As4()
	a.bytes(unix.IFA_LOCAL, ip[:])
	a.bytes(unix.IFA_ADDRESS, ip[:])
	if err := rt.do(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, append(msg, a...)); err != nil {
		return fmt.Errorf("add address %s: %w", p, err)
	}
	return nil
}

func nlDelAddr(rt *nlSock, index uint32, p netip.Prefix) error {
	msg := make([]byte, unix.SizeofIfAddrmsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())
	msg[3] = unix.RT_SCOPE_UNIVERSE
	binary.NativeEndian.PutUint32(msg[4:], index)
	var a nlAttrs
	ip := p.Addr().As4()
	a.bytes(unix.IFA_LOCAL, ip[:])
	a.bytes(unix.IFA_ADDRESS, ip[:])
	if err := rt.do(unix.RTM_DELADDR, 0, append(msg, a...)); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		return fmt.Errorf("remove address %s: %w", p, err)
	}
	return nil
}

func nlAddRoute(rt *nlSock, p netip.Prefix, index uint32) error {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = unix.RTPROT_BOOT
	msg[6] = unix.RT_SCOPE_LINK
	msg[7] = unix.RTN_UNICAST
	var a nlAttrs
	ip := p.Addr().As4()
	a.bytes(unix.RTA_DST, ip[:])
	a.u32(unix.RTA_OIF, index)
	if err := rt.do(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, append(msg, a...)); err != nil {
		return fmt.Errorf("add route %s: %w", p, err)
	}
	return nil
}

func nlDelRoute(rt *nlSock, p netip.Prefix, index uint32) error {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = unix.RTPROT_BOOT
	msg[6] = unix.RT_SCOPE_LINK
	msg[7] = unix.RTN_UNICAST
	var a nlAttrs
	ip := p.Addr().As4()
	a.bytes(unix.RTA_DST, ip[:])
	a.u32(unix.RTA_OIF, index)
	if err := rt.do(unix.RTM_DELROUTE, 0, append(msg, a...)); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("remove route %s: %w", p, err)
	}
	return nil
}

func nlListBootRoutes(rt *nlSock, index uint32) ([]netip.Prefix, error) {
	req := make([]byte, unix.SizeofRtMsg)
	req[0] = unix.AF_INET
	req[4] = unix.RT_TABLE_MAIN
	replies, err := rt.request(unix.RTM_GETROUTE, unix.NLM_F_DUMP, req)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	var out []netip.Prefix
	for _, body := range replies {
		if len(body) < unix.SizeofRtMsg {
			continue
		}
		family := body[0]
		dstLen := int(body[1])
		table := body[4]
		proto := body[5]
		rType := body[7]
		if family != unix.AF_INET || table != unix.RT_TABLE_MAIN || proto != unix.RTPROT_BOOT || rType != unix.RTN_UNICAST || dstLen == 0 || dstLen > 32 {
			continue
		}
		var (
			oif   uint32
			hasIF bool
			dst   netip.Addr
		)
		nlParseAttrs(body[unix.SizeofRtMsg:], func(typ uint16, val []byte) {
			switch typ {
			case unix.RTA_OIF:
				if len(val) >= 4 {
					oif = binary.NativeEndian.Uint32(val)
					hasIF = true
				}
			case unix.RTA_DST:
				if len(val) == 4 {
					dst = netip.AddrFrom4([4]byte(val))
				}
			}
		})
		if hasIF && oif == index && dst.IsValid() {
			out = append(out, netip.PrefixFrom(dst, dstLen).Masked())
		}
	}
	return out, nil
}
