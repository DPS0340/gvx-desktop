package overlay

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

// newKey returns a WireGuard private key and its public key.
func newKey() (priv, pub []byte, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return k.Bytes(), k.PublicKey().Bytes(), nil
}

// router is the hub's TUN device. A packet a container sends comes out of
// the WireGuard device through Write; the router keeps it when its source
// and destination share a network, and hands it back through Read, where
// the device sends it to the peer whose allowed IPs hold the destination.
// WireGuard has already checked that the source is the sending peer's.
type router struct {
	allow   func(src, dst netip.Addr) bool
	deliver func(dst netip.Addr, pkt []byte) bool
	queue   chan []byte
	events  chan tun.Event
	closing chan struct{}
	once    sync.Once
}

// LinkMTU is the MTU of the containers' links and the hub. Each packet
// costs a trip through both sandboxes' NAT relays and a system call in the
// hub, whatever its size, so a large MTU carries more for the same work:
// 200 MB went across at 47 MB/s with 1420, 160-180 MB/s with 8920 and
// 260 MB/s with 60000. The sandbox's eth0 needs NATOverhead more (rungv
// -tune net.nat.mtu).
var LinkMTU = 60000

// BaseMTU is the link MTU for a sandbox whose eth0 keeps rungv's 1500.
const BaseMTU = 1420

// NATOverhead is what WireGuard over IPv4 adds to a packet (60 bytes),
// with room for IPv6 as Linux leaves.
const NATOverhead = 80

const (
	routerBatch = 32
	// routerQueued bounds the router's queue, 15 MB at LinkMTU.
	routerQueued = 256
)

func newRouter(allow func(src, dst netip.Addr) bool, deliver func(dst netip.Addr, pkt []byte) bool) *router {
	r := &router{
		allow:   allow,
		deliver: deliver,
		queue:   make(chan []byte, routerQueued),
		events:  make(chan tun.Event, 1),
		closing: make(chan struct{}),
	}
	r.events <- tun.EventUp
	return r
}

func (r *router) File() *os.File           { return nil }
func (r *router) MTU() (int, error)        { return LinkMTU, nil }
func (r *router) Name() (string, error)    { return "gvx-hub", nil }
func (r *router) Events() <-chan tun.Event { return r.events }
func (r *router) BatchSize() int           { return routerBatch }

func (r *router) Close() error {
	r.once.Do(func() {
		close(r.closing)
		close(r.events)
	})
	return nil
}

func (r *router) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	var p []byte
	select {
	case p = <-r.queue:
	case <-r.closing:
		return 0, os.ErrClosed
	}
	n := 0
	for {
		sizes[n] = copy(bufs[n][offset:], p)
		n++
		if n == len(bufs) {
			return n, nil
		}
		select {
		case p = <-r.queue:
		default:
			return n, nil
		}
	}
}

func (r *router) enqueue(p []byte) {
	if r == nil {
		return
	}
	select {
	case <-r.closing:
		return
	default:
	}
	select {
	case r.queue <- append([]byte(nil), p...):
	case <-r.closing:
	default: // full: drop, as a congested link does
	}
}

func (r *router) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		p := b[offset:]
		src, dst, ok := ipv4Addrs(p)
		if !ok || !r.allow(src, dst) {
			continue
		}
		if r.deliver != nil && r.deliver(dst, p) {
			continue
		}
		r.enqueue(p)
	}
	return len(bufs), nil
}

// ipv4Addrs reads the source and destination of an IPv4 packet.
func ipv4Addrs(p []byte) (src, dst netip.Addr, ok bool) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return src, dst, false
	}
	return netip.AddrFrom4([4]byte(p[12:16])), netip.AddrFrom4([4]byte(p[16:20])), true
}

// loopbackBind is a WireGuard Bind on one UDP socket of 127.0.0.1. The
// sandboxes reach it at the NAT's host address, which is the host's
// loopback; nothing else needs to, and listening only there keeps the
// firewall from asking about the app.
type loopbackBind struct {
	mu sync.Mutex
	pc *net.UDPConn
}

type loopbackEndpoint netip.AddrPort

func (e loopbackEndpoint) ClearSrc()           {}
func (e loopbackEndpoint) SrcToString() string { return "" }
func (e loopbackEndpoint) DstToString() string { return netip.AddrPort(e).String() }
func (e loopbackEndpoint) DstToBytes() []byte  { b, _ := netip.AddrPort(e).MarshalBinary(); return b }
func (e loopbackEndpoint) DstIP() netip.Addr   { return netip.AddrPort(e).Addr() }
func (e loopbackEndpoint) SrcIP() netip.Addr   { return netip.Addr{} }

func (b *loopbackBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pc != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	pc, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)))
	if err != nil {
		return nil, 0, err
	}
	disableUDPConnReset(pc)
	// Room for bursts of large datagrams; macOS starts a UDP socket with
	// a send buffer of 9216 bytes, less than two packets at LinkMTU.
	_ = pc.SetReadBuffer(4 << 20)
	_ = pc.SetWriteBuffer(1 << 20)
	b.pc = pc
	recv := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for {
			n, addr, err := pc.ReadFromUDPAddrPort(packets[0])
			if err != nil {
				if isPlatformTransientUDPErr(err) {
					continue
				}
				return 0, err
			}
			sizes[0] = n
			eps[0] = loopbackEndpoint(netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()))
			return 1, nil
		}
	}
	return []conn.ReceiveFunc{recv}, uint16(pc.LocalAddr().(*net.UDPAddr).Port), nil
}

func (b *loopbackBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pc == nil {
		return nil
	}
	err := b.pc.Close()
	b.pc = nil
	return err
}

func (b *loopbackBind) SetMark(uint32) error { return nil }
func (b *loopbackBind) BatchSize() int       { return 1 }

func (b *loopbackBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.mu.Lock()
	pc := b.pc
	b.mu.Unlock()
	if pc == nil {
		return net.ErrClosed
	}
	e, ok := ep.(loopbackEndpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	for _, buf := range bufs {
		if _, err := pc.WriteToUDPAddrPort(buf, netip.AddrPort(e)); err != nil {
			return err
		}
	}
	return nil
}

func (b *loopbackBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return loopbackEndpoint(ap), nil
}

// hub is the WireGuard device every container's link has as its one peer.
type hub struct {
	dev    *device.Device
	router *router
	pub    []byte
	port   uint16
}

func startHub(allow func(src, dst netip.Addr) bool, deliver func(dst netip.Addr, pkt []byte) bool, logf func(string, ...any)) (*hub, error) {
	priv, pub, err := newKey()
	if err != nil {
		return nil, err
	}
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf:   func(format string, args ...any) { logf("overlay: wireguard: "+format, args...) },
	}
	bind := &loopbackBind{}
	r := newRouter(allow, deliver)
	dev := device.NewDevice(r, bind, logger)
	if err := dev.IpcSet("private_key=" + hex.EncodeToString(priv) + "\nlisten_port=0\n"); err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, err
	}
	bind.mu.Lock()
	pc := bind.pc
	bind.mu.Unlock()
	if pc == nil {
		dev.Close()
		return nil, errors.New("overlay: the hub did not open its socket")
	}
	return &hub{dev: dev, router: r, pub: pub, port: uint16(pc.LocalAddr().(*net.UDPAddr).Port)}, nil
}

// setPeer adds or updates a container's peer with its addresses.
func (h *hub) setPeer(pub []byte, addrs []netip.Addr) error {
	var cfg strings.Builder
	fmt.Fprintf(&cfg, "public_key=%s\nreplace_allowed_ips=true\n", hex.EncodeToString(pub))
	for _, a := range addrs {
		fmt.Fprintf(&cfg, "allowed_ip=%s\n", netip.PrefixFrom(a, a.BitLen()))
	}
	return h.dev.IpcSet(cfg.String())
}

func (h *hub) removePeer(pub []byte) error {
	return h.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", hex.EncodeToString(pub)))
}

func (h *hub) close() { h.dev.Close() }
