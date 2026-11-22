//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

// setupLink makes the WireGuard link of cfg with rtnetlink and the
// wireguard generic netlink family, as ip(8) and wg(8) would.
func setupLink(cfg guestcfg.Config) error {
	endpoint, err := netip.ParseAddrPort(cfg.Endpoint)
	if err != nil || !endpoint.Addr().Is4() {
		return fmt.Errorf("hub endpoint %q is not an IPv4 address and port", cfg.Endpoint)
	}
	var addrs, routes []netip.Prefix
	for _, a := range cfg.Addresses {
		p, err := netip.ParsePrefix(a)
		if err != nil || !p.Addr().Is4() {
			return fmt.Errorf("address %q is not an IPv4 prefix", a)
		}
		addrs = append(addrs, p)
	}
	for _, r := range cfg.Routes {
		p, err := netip.ParsePrefix(r)
		if err != nil || !p.Addr().Is4() {
			return fmt.Errorf("route %q is not an IPv4 prefix", r)
		}
		routes = append(routes, p.Masked())
	}
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1420
	}

	rt, err := dial(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer rt.close()

	// ip link add gvx0 mtu N type wireguard
	var b attrs
	b.str(unix.IFLA_IFNAME, guestcfg.LinkName)
	b.u32(unix.IFLA_MTU, uint32(mtu))
	// rtnetlink nests without NLA_F_NESTED, as ip(8) does; gVisor
	// refuses IFLA_LINKINFO with the flag.
	var info attrs
	info.str(unix.IFLA_INFO_KIND, "wireguard")
	b.bytes(unix.IFLA_LINKINFO, info)
	ifi := make([]byte, unix.SizeofIfInfomsg)
	if err := rt.do(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, append(ifi, b...)); err != nil {
		return fmt.Errorf("create link %s: %w", guestcfg.LinkName, err)
	}
	iface, err := net.InterfaceByName(guestcfg.LinkName)
	if err != nil {
		return fmt.Errorf("find link %s: %w", guestcfg.LinkName, err)
	}
	index := uint32(iface.Index)

	if err := configureWireGuard(index, cfg, endpoint, addrs, routes); err != nil {
		return err
	}

	// ip addr add A/N dev gvx0
	for _, p := range addrs {
		msg := make([]byte, unix.SizeofIfAddrmsg)
		msg[0] = unix.AF_INET
		msg[1] = byte(p.Bits())
		msg[3] = unix.RT_SCOPE_UNIVERSE
		binary.NativeEndian.PutUint32(msg[4:], index)
		var a attrs
		ip := p.Addr().As4()
		a.bytes(unix.IFA_LOCAL, ip[:])
		a.bytes(unix.IFA_ADDRESS, ip[:])
		if err := rt.do(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, append(msg, a...)); err != nil {
			return fmt.Errorf("add address %s: %w", p, err)
		}
	}

	// ip link set gvx0 up
	up := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(up[4:], index)
	binary.NativeEndian.PutUint32(up[8:], unix.IFF_UP)
	binary.NativeEndian.PutUint32(up[12:], unix.IFF_UP)
	if err := rt.do(unix.RTM_NEWLINK, 0, up); err != nil {
		return fmt.Errorf("set link %s up: %w", guestcfg.LinkName, err)
	}

	// ip route add P dev gvx0
	for _, p := range routes {
		covered := false
		for _, a := range addrs {
			if a.Masked() == p {
				covered = true
			}
		}
		if covered {
			continue
		}
		if err := addRoute(rt, p, index); err != nil {
			return err
		}
	}
	return nil
}

// addRoute adds "ip route add P dev INDEX".
func addRoute(rt *nlsock, p netip.Prefix, index uint32) error {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = unix.RTPROT_BOOT
	msg[6] = unix.RT_SCOPE_LINK
	msg[7] = unix.RTN_UNICAST
	var a attrs
	ip := p.Addr().As4()
	a.bytes(unix.RTA_DST, ip[:])
	a.u32(unix.RTA_OIF, index)
	if err := rt.do(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, append(msg, a...)); err != nil {
		return fmt.Errorf("add route %s: %w", p, err)
	}
	return nil
}

// isolate takes the default route of eth0 away and keeps routes to the
// addresses in keep, the NAT's host (the hub) and DNS relay.
func isolate(keep []netip.Addr) error {
	eth, err := net.InterfaceByName("eth0")
	if err != nil {
		return fmt.Errorf("find eth0: %w", err)
	}
	rt, err := dial(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer rt.close()
	for _, a := range keep {
		if err := addRoute(rt, netip.PrefixFrom(a, 32), uint32(eth.Index)); err != nil {
			return err
		}
	}
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[4] = unix.RT_TABLE_MAIN
	msg[6] = unix.RT_SCOPE_NOWHERE
	var a attrs
	a.u32(unix.RTA_OIF, uint32(eth.Index))
	if err := rt.do(unix.RTM_DELROUTE, 0, append(msg, a...)); err != nil {
		return fmt.Errorf("remove the default route: %w", err)
	}
	return nil
}

// Constants of include/uapi/linux/wireguard.h.
const (
	wgCmdSetDevice        = 1
	wgDeviceAIfindex      = 1
	wgDeviceAPrivateKey   = 3
	wgDeviceAFlags        = 5
	wgDeviceAPeers        = 8
	wgDeviceFReplacePeers = 1
	wgPeerAPublicKey      = 1
	wgPeerAFlags          = 3
	wgPeerAEndpoint       = 4
	wgPeerAKeepalive      = 5
	wgPeerAAllowedIPs     = 9
	wgPeerFReplaceIPs     = 2
	wgAllowedIPAFamily    = 1
	wgAllowedIPAIPAddr    = 2
	wgAllowedIPACidrMask  = 3
)

// configureWireGuard sets the device's key and its one peer, the hub,
// which carries the container's networks.
func configureWireGuard(index uint32, cfg guestcfg.Config, endpoint netip.AddrPort, addrs, routes []netip.Prefix) error {
	gn, err := dial(unix.NETLINK_GENERIC)
	if err != nil {
		return err
	}
	defer gn.close()
	family, err := gn.family("wireguard")
	if err != nil {
		return fmt.Errorf("the wireguard netlink family: %w", err)
	}

	allowed := map[netip.Prefix]bool{}
	for _, p := range addrs {
		allowed[p.Masked()] = true
	}
	for _, p := range routes {
		allowed[p] = true
	}

	var b attrs
	b.u32(wgDeviceAIfindex, index)
	b.bytes(wgDeviceAPrivateKey, cfg.PrivateKey)
	b.u32(wgDeviceAFlags, wgDeviceFReplacePeers)
	b.nest(wgDeviceAPeers, func(b *attrs) {
		b.nest(0, func(b *attrs) {
			b.bytes(wgPeerAPublicKey, cfg.HubPublicKey)
			b.u32(wgPeerAFlags, wgPeerFReplaceIPs)
			sa := make([]byte, 16)
			binary.NativeEndian.PutUint16(sa[0:], unix.AF_INET)
			binary.BigEndian.PutUint16(sa[2:], endpoint.Port())
			ip := endpoint.Addr().As4()
			copy(sa[4:], ip[:])
			b.bytes(wgPeerAEndpoint, sa)
			if cfg.Keepalive > 0 {
				b.u16(wgPeerAKeepalive, uint16(cfg.Keepalive))
			}
			b.nest(wgPeerAAllowedIPs, func(b *attrs) {
				for p := range allowed {
					b.nest(0, func(b *attrs) {
						b.u16(wgAllowedIPAFamily, unix.AF_INET)
						ip := p.Addr().As4()
						b.bytes(wgAllowedIPAIPAddr, ip[:])
						b.u8(wgAllowedIPACidrMask, uint8(p.Bits()))
					})
				}
			})
		})
	})
	msg := append([]byte{wgCmdSetDevice, 1, 0, 0}, b...)
	if err := gn.do(family, 0, msg); err != nil {
		return fmt.Errorf("configure %s: %w", guestcfg.LinkName, err)
	}
	return nil
}

// ---- netlink ----

type nlsock struct {
	fd  int
	seq uint32
}

func dial(proto int) (*nlsock, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	return &nlsock{fd: fd}, nil
}

func (s *nlsock) close() { unix.Close(s.fd) }

// request sends one message and returns the replies up to its ack.
func (s *nlsock) request(typ uint16, flags uint16, payload []byte) ([][]byte, error) {
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
			data = data[min(nlmAlign(l), len(data)):]
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

func (s *nlsock) do(typ uint16, flags uint16, payload []byte) error {
	_, err := s.request(typ, flags, payload)
	return err
}

// family resolves a generic netlink family's ID.
func (s *nlsock) family(name string) (uint16, error) {
	var b attrs
	b.str(unix.CTRL_ATTR_FAMILY_NAME, name)
	replies, err := s.request(unix.GENL_ID_CTRL, 0, append([]byte{unix.CTRL_CMD_GETFAMILY, 1, 0, 0}, b...))
	if err != nil {
		return 0, err
	}
	for _, r := range replies {
		if len(r) < 4 {
			continue
		}
		for typ, val := range parseAttrs(r[4:]) {
			if typ == unix.CTRL_ATTR_FAMILY_ID && len(val) >= 2 {
				return binary.NativeEndian.Uint16(val), nil
			}
		}
	}
	return 0, errors.New("not found")
}

func nlmAlign(n int) int { return (n + 3) &^ 3 }

type attrs []byte

func (b *attrs) bytes(typ uint16, val []byte) {
	h := make([]byte, 4)
	binary.NativeEndian.PutUint16(h[0:], uint16(4+len(val)))
	binary.NativeEndian.PutUint16(h[2:], typ)
	*b = append(*b, h...)
	*b = append(*b, val...)
	for len(*b)%4 != 0 {
		*b = append(*b, 0)
	}
}

func (b *attrs) str(typ uint16, s string) { b.bytes(typ, append([]byte(s), 0)) }
func (b *attrs) u8(typ uint16, v uint8)   { b.bytes(typ, []byte{v}) }

func (b *attrs) u16(typ uint16, v uint16) {
	val := make([]byte, 2)
	binary.NativeEndian.PutUint16(val, v)
	b.bytes(typ, val)
}

func (b *attrs) u32(typ uint16, v uint32) {
	val := make([]byte, 4)
	binary.NativeEndian.PutUint32(val, v)
	b.bytes(typ, val)
}

// nest adds a nested attribute with NLA_F_NESTED, as generic netlink
// families expect.
func (b *attrs) nest(typ uint16, fill func(*attrs)) {
	var inner attrs
	fill(&inner)
	b.bytes(typ|unix.NLA_F_NESTED, inner)
}

// parseAttrs yields the top-level attributes of data.
func parseAttrs(data []byte) func(yield func(uint16, []byte) bool) {
	return func(yield func(uint16, []byte) bool) {
		for len(data) >= 4 {
			l := int(binary.NativeEndian.Uint16(data[0:]))
			typ := binary.NativeEndian.Uint16(data[2:]) &^ (unix.NLA_F_NESTED | unix.NLA_F_NET_BYTEORDER)
			if l < 4 || l > len(data) {
				return
			}
			if !yield(typ, data[4:l]) {
				return
			}
			data = data[min(nlmAlign(l), len(data)):]
		}
	}
}
