// Package overlay joins the experimental sandbox engine's containers on
// user networks. Every sandbox has a NAT of its own, so containers cannot
// reach each other through it. Each container on a user network gets a
// WireGuard link (made in the guest by netinit) whose one peer is a hub in
// the app; the hub passes packets between containers that share a network.
// A DNS server per container answers the names of those containers and
// passes other names to the host's resolvers.
//
// The NAT stays as it was: the internet, published ports and
// host.docker.internal do not go through the hub.
package overlay

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

// Pool is where networks get their subnets, one /24 each, unless a client
// asks for its own. Podman uses the same range for its networks.
var Pool = netip.MustParsePrefix("10.89.0.0/16")

// HostGateway is the host as the sandboxes' NAT shows it: a guest that
// connects there reaches the host's 127.0.0.1.
const HostGateway = "10.0.2.2"

// keepalive keeps each sandbox's NAT entry for the hub open, in seconds.
const keepalive = 25

// Member is a container on one or more user networks, as the engine
// records it.
type Member struct {
	ID string
	// Endpoints are the container's places, by network ID.
	Endpoints map[string]Endpoint
}

// Endpoint is a container's address and DNS names on one network.
type Endpoint struct {
	Addr  netip.Addr
	Names []string
}

// Fabric is the hub and the containers' DNS servers. The zero value is not
// usable; call New.
type Fabric struct {
	logf func(string, ...any)
	up   *upstreams

	mu      sync.Mutex
	hub     *hub
	peers   map[string]*peer // by container ID
	members map[string]Member
	closed  bool

	view atomic.Pointer[view]
}

// peer is a container that joined: a running sandbox with a link.
type peer struct {
	pub  []byte
	live []netip.Addr // addresses configured on its link
	dns  *dnsServer
}

// view is what packets and queries are checked against, rebuilt on every
// change.
type view struct {
	owner   map[netip.Addr]string          // live address → container
	nets    map[string]map[string]struct{} // container → network IDs
	members map[string]Member
	live    map[string][]netip.Addr
}

// New returns a Fabric. It starts nothing until a container joins.
func New(logf func(string, ...any)) *Fabric {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	f := &Fabric{
		logf:    logf,
		up:      &upstreams{discover: hostDNSServers},
		peers:   map[string]*peer{},
		members: map[string]Member{},
	}
	f.view.Store(&view{})
	return f
}

// JoinOptions are how a container joins.
type JoinOptions struct {
	// Isolated is a container only on internal networks: its DNS server
	// answers the containers' names and refuses the others, as Docker's
	// does on an internal network.
	Isolated bool
	// MTU is the link's MTU; 0 is LinkMTU.
	MTU int
}

// Join starts a container's side of the hub: a peer for addrs, the
// addresses its link will have, and a DNS server. It returns the guest's
// configuration and the DNS server's address for rungv -host-dns. A
// container that joined before is joined again with a new key.
func (f *Fabric) Join(id string, addrs []netip.Prefix, opts JoinOptions) (guestcfg.Config, netip.AddrPort, error) {
	if len(addrs) == 0 {
		return guestcfg.Config{}, netip.AddrPort{}, errors.New("overlay: no addresses")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return guestcfg.Config{}, netip.AddrPort{}, errors.New("overlay: closed")
	}
	if f.hub == nil {
		h, err := startHub(f.allow, f.logf)
		if err != nil {
			return guestcfg.Config{}, netip.AddrPort{}, err
		}
		f.hub = h
	}
	f.leaveLocked(id)

	priv, pub, err := newKey()
	if err != nil {
		return guestcfg.Config{}, netip.AddrPort{}, err
	}
	live := make([]netip.Addr, 0, len(addrs))
	strs := make([]string, 0, len(addrs))
	routes := []string{Pool.String()}
	for _, p := range addrs {
		live = append(live, p.Addr())
		strs = append(strs, p.String())
		if !Pool.Contains(p.Addr()) {
			routes = append(routes, p.Masked().String())
		}
	}
	if err := f.hub.setPeer(pub, live); err != nil {
		return guestcfg.Config{}, netip.AddrPort{}, err
	}
	up := f.up
	if opts.Isolated {
		up = nil
	}
	dns, err := startDNS(func(name string) []netip.Addr { return f.resolve(id, name) }, up)
	if err != nil {
		_ = f.hub.removePeer(pub)
		return guestcfg.Config{}, netip.AddrPort{}, err
	}
	f.peers[id] = &peer{pub: pub, live: live, dns: dns}
	mtu := opts.MTU
	if mtu <= 0 {
		mtu = LinkMTU
	}
	f.rebuildLocked()

	return guestcfg.Config{
		PrivateKey:   priv,
		HubPublicKey: f.hub.pub,
		Endpoint:     netip.AddrPortFrom(netip.MustParseAddr(HostGateway), f.hub.port).String(),
		Addresses:    strs,
		Routes:       routes,
		MTU:          mtu,
		Keepalive:    keepalive,
	}, dns.addr, nil
}

// Leave takes a container off the hub, when its sandbox stopped.
func (f *Fabric) Leave(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaveLocked(id) {
		f.rebuildLocked()
	}
}

func (f *Fabric) leaveLocked(id string) bool {
	p, ok := f.peers[id]
	if !ok {
		return false
	}
	delete(f.peers, id)
	if f.hub != nil {
		if err := f.hub.removePeer(p.pub); err != nil {
			f.logf("overlay: remove peer: %v", err)
		}
	}
	go p.dns.close()
	return true
}

// SetMembers replaces what the Fabric knows of the containers' networks:
// which containers share a network, and their names. The engine calls it
// after every change.
func (f *Fabric) SetMembers(members []Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = make(map[string]Member, len(members))
	for _, m := range members {
		f.members[m.ID] = m
	}
	f.rebuildLocked()
}

func (f *Fabric) rebuildLocked() {
	v := &view{
		owner:   map[netip.Addr]string{},
		nets:    map[string]map[string]struct{}{},
		members: f.members,
		live:    map[string][]netip.Addr{},
	}
	for id, p := range f.peers {
		for _, a := range p.live {
			v.owner[a] = id
		}
		v.live[id] = p.live
	}
	for id, m := range f.members {
		set := make(map[string]struct{}, len(m.Endpoints))
		for netID := range m.Endpoints {
			set[netID] = struct{}{}
		}
		v.nets[id] = set
	}
	f.view.Store(v)
}

// Close stops the hub and the DNS servers.
func (f *Fabric) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for id := range f.peers {
		f.leaveLocked(id)
	}
	if f.hub != nil {
		f.hub.close()
		f.hub = nil
	}
	f.rebuildLocked()
}

// CanReach reports whether a packet from src may reach dst: both are
// containers that joined and share a network.
func (f *Fabric) CanReach(src, dst netip.Addr) bool { return f.allow(src, dst) }

func (f *Fabric) allow(src, dst netip.Addr) bool {
	v := f.view.Load()
	a, ok := v.owner[src]
	if !ok {
		return false
	}
	b, ok := v.owner[dst]
	if !ok {
		return false
	}
	return shares(v.nets[a], v.nets[b])
}

func shares(a, b map[string]struct{}) bool {
	for n := range a {
		if _, ok := b[n]; ok {
			return true
		}
	}
	return false
}

// resolve answers name for the container asker: the addresses of the
// running containers that share a network with it and have the name
// there. A container answers with its address on that network when its
// link has it, and with another of its link's addresses when it joined the
// network after it started.
func (f *Fabric) resolve(asker, name string) []netip.Addr {
	v := f.view.Load()
	mine := v.nets[asker]
	if len(mine) == 0 {
		return nil
	}
	var out []netip.Addr
	for id, m := range v.members {
		live := v.live[id]
		if len(live) == 0 {
			continue
		}
		for netID, ep := range m.Endpoints {
			if _, ok := mine[netID]; !ok {
				continue
			}
			if !slices.ContainsFunc(ep.Names, func(n string) bool { return strings.EqualFold(n, name) }) {
				continue
			}
			addr := ep.Addr
			if !slices.Contains(live, addr) {
				addr = live[0]
			}
			if !slices.Contains(out, addr) {
				out = append(out, addr)
			}
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out
}
