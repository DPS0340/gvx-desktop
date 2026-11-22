package engine

// Containers on user networks reach each other through the overlay
// (internal/overlay): a WireGuard link in each sandbox, made by netinit,
// to a hub in the app, and a DNS server per container. See
// docs/design/sandbox-engine.md, "Networks between containers".

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ironpark/gvx-desktop/internal/overlay"
	"github.com/ironpark/gvx-desktop/internal/overlay/guest"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

type overlayState struct {
	mu  sync.Mutex
	fab *overlay.Fabric

	binOnce sync.Once
	binPath string
	binErr  error
}

// netinit is guest.Netinit; the tests replace it.
var netinit = guest.Netinit

// overlayWarning is the create warning for a container on n when the
// overlay cannot join it: this build has no netinit.
func overlayWarning(n Network) string {
	if n.Builtin || n.Subnet == "" {
		return ""
	}
	if _, err := netinit(); err != nil {
		return fmt.Sprintf("Containers on the network %q cannot reach each other: %v", n.Name, err)
	}
	return ""
}

// fabric returns the overlay, making it on first use.
func (e *Engine) fabric() *overlay.Fabric {
	e.ovl.mu.Lock()
	defer e.ovl.mu.Unlock()
	if e.ovl.fab == nil {
		e.ovl.fab = overlay.New(e.logf)
	}
	return e.ovl.fab
}

// closeOverlay stops the hub and the DNS servers, after the containers
// stopped.
func (e *Engine) closeOverlay() {
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	e.ovl.fab = nil
	e.ovl.mu.Unlock()
	if fab != nil {
		fab.Close()
	}
}

// netinitPath writes netinit into the engine's directory, once, for the
// sandboxes to mount.
func (e *Engine) netinitPath() (string, error) {
	e.ovl.binOnce.Do(func() {
		bin, err := netinit()
		if err != nil {
			e.ovl.binErr = err
			return
		}
		p := e.path("netinit")
		if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, bin) {
			e.ovl.binPath = p
			return
		}
		tmp, err := os.CreateTemp(e.path(), ".netinit-*")
		if err != nil {
			e.ovl.binErr = err
			return
		}
		_, werr := tmp.Write(bin)
		cerr := tmp.Close()
		if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o755), os.Rename(tmp.Name(), p)); err != nil {
			os.Remove(tmp.Name())
			e.ovl.binErr = err
			return
		}
		e.ovl.binPath = p
	})
	return e.ovl.binPath, e.ovl.binErr
}

// extraHostLines are the /etc/hosts lines of docker's extra hosts.
func extraHostLines(hosts []string) ([]string, error) {
	var out []string
	for _, h := range hosts {
		l, err := guestcfg.ParseExtraHost(h, overlay.HostGateway)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// overlayAddrs are c's addresses on user networks, as its link will have
// them, and whether those networks are all internal, with no other network
// (the builtin bridge) besides.
func (e *Engine) overlayAddrs(c *Container) (addrs []netip.Prefix, isolated bool) {
	if c.HostConfig.NetworkMode == "none" {
		return nil, false
	}
	e.net.mu.Lock()
	defer e.net.mu.Unlock()
	var out []netip.Prefix
	isolated = true
	for _, n := range e.net.byID {
		if _, ok := n.Containers[c.ID]; !ok {
			continue
		}
		if n.Builtin || !n.Internal {
			isolated = false
		}
		if n.Builtin {
			continue
		}
		sub, err1 := netip.ParsePrefix(n.Subnet)
		a, err2 := netip.ParseAddr(n.IPs[c.ID])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, netip.PrefixFrom(a, sub.Bits()))
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return out, isolated && len(out) > 0
}

// joinOverlay joins c to the overlay for the start whose spec is spec, and
// returns the spec that runs netinit in front of the container's command.
// A container on no user network and with no extra hosts keeps spec. When
// netinit cannot run or the overlay cannot join it, the container starts
// without them and the reason goes to the log.
func (e *Engine) joinOverlay(c *Container, spec sandbox.Spec) sandbox.Spec {
	addrs, isolated := e.overlayAddrs(c)
	hosts, err := extraHostLines(c.HostConfig.ExtraHosts)
	if err != nil {
		e.logf("engine: container %s: %v", shortHex(c.ID), err)
		hosts = nil
	}
	if len(addrs) == 0 && len(hosts) == 0 {
		return spec
	}
	bin, err := e.netinitPath()
	if err != nil {
		e.logf("engine: container %s starts without its networks and extra hosts: %v", shortHex(c.ID), err)
		return spec
	}
	var (
		cfg    guestcfg.Config
		dns    netip.AddrPort
		joined bool
	)
	if len(addrs) > 0 {
		cfg, dns, err = e.fabric().Join(c.ID, addrs, overlay.JoinOptions{Isolated: isolated, MTU: e.linkMTU()})
		if err != nil {
			e.logf("engine: container %s starts without its networks: %v", shortHex(c.ID), err)
			cfg, dns = guestcfg.Config{}, netip.AddrPort{}
		} else {
			joined = true
			cfg.Isolated = isolated
		}
	}
	cfg.Hosts = hosts
	if !cfg.HasLink() && len(cfg.Hosts) == 0 {
		return spec
	}
	out, err := withOverlay(spec, cfg, bin, dns)
	if err != nil {
		if joined {
			e.leaveOverlay(c.ID)
		}
		e.logf("engine: container %s starts without its networks and extra hosts: %v", shortHex(c.ID), err)
		return spec
	}
	if joined {
		e.syncOverlay()
	}
	return out
}

// withOverlay returns spec running netinit (mounted from bin) with cfg in
// front of its command, as root, and with the DNS server dns when it is
// valid.
func withOverlay(spec sandbox.Spec, cfg guestcfg.Config, bin string, dns netip.AddrPort) (sandbox.Spec, error) {
	if !spec.SetEntrypoint || spec.Entrypoint == "" {
		return spec, errors.New("the container has no command")
	}
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return spec, err
	}
	cfg.User = spec.User
	cfg.KeepHome = slices.ContainsFunc(spec.Env, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "HOME"
	})
	enc, err := cfg.Encode()
	if err != nil {
		return spec, err
	}
	out := spec.Clone()
	out.Entrypoint = guestcfg.Path
	out.Command = rungv.FormatArgs(append([]string{spec.Entrypoint}, args...))
	out.User = ""
	out.Env = append(slices.DeleteFunc(slices.Clone(spec.Env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == guestcfg.EnvVar
	}), guestcfg.EnvVar+"="+enc)
	out.Mounts = append(slices.Clone(spec.Mounts), sandbox.Mount{
		Type:     sandbox.MountBind,
		Source:   bin,
		Target:   guestcfg.Path,
		ReadOnly: true,
	})
	if dns.IsValid() || cfg.HasLink() {
		extra, err := rungv.SplitArgs(spec.ExtraArgs)
		if err != nil {
			return spec, err
		}
		if dns.IsValid() {
			extra = append(extra, "-host-dns", dns.String())
		}
		if cfg.HasLink() && cfg.MTU > 0 {
			if nat := cfg.MTU + overlay.NATOverhead; nat > 1500 {
				extra = append(extra, "-tune", "net.nat.mtu="+strconv.Itoa(nat))
			}
		}
		out.ExtraArgs = rungv.FormatArgs(extra)
	}
	return out, nil
}

// linkMTU is overlay.LinkMTU when rungv can raise eth0's MTU for it
// (-tune), and overlay.BaseMTU when it cannot.
func (e *Engine) linkMTU() int {
	if r, ok := e.runner.(interface{ HasRunFlag(string) bool }); ok && !r.HasRunFlag("tune") {
		return overlay.BaseMTU
	}
	return overlay.LinkMTU
}

// leaveOverlay takes a container off the overlay when its sandbox stopped.
func (e *Engine) leaveOverlay(id string) {
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	e.ovl.mu.Unlock()
	if fab != nil {
		fab.Leave(id)
	}
}

// syncOverlay tells the overlay which containers share which networks, and
// their names, after a change.
func (e *Engine) syncOverlay() {
	e.ovl.mu.Lock()
	fab := e.ovl.fab
	e.ovl.mu.Unlock()
	if fab == nil {
		return
	}
	names := map[string]string{}
	e.ctr.mu.Lock()
	for id, c := range e.ctr.byID {
		names[id] = c.Name
	}
	e.ctr.mu.Unlock()

	members := map[string]*overlay.Member{}
	e.net.mu.Lock()
	for _, n := range e.net.byID {
		if n.Builtin {
			continue
		}
		for cid, aliases := range n.Containers {
			a, err := netip.ParseAddr(n.IPs[cid])
			if err != nil {
				continue
			}
			name, ok := names[cid]
			if !ok {
				continue
			}
			m := members[cid]
			if m == nil {
				m = &overlay.Member{ID: cid, Endpoints: map[string]overlay.Endpoint{}}
				members[cid] = m
			}
			m.Endpoints[n.ID] = overlay.Endpoint{Addr: a, Names: buildDNSNames(name, shortHex(cid), aliases)}
		}
	}
	e.net.mu.Unlock()

	list := make([]overlay.Member, 0, len(members))
	for _, id := range slices.Sorted(maps.Keys(members)) {
		list = append(list, *members[id])
	}
	fab.SetMembers(list)
}

// reconcileEndpoints makes the containers' endpoints on user networks show
// the addresses their networks gave them: containers made before the
// overlay recorded the NAT's address.
func (e *Engine) reconcileEndpoints() {
	type addr struct {
		ip, gateway string
		bits        int
	}
	byNet := map[string]map[string]addr{} // network name → container → addr
	e.net.mu.Lock()
	for _, n := range e.net.byID {
		sub, err := netip.ParsePrefix(n.Subnet)
		if n.Builtin || err != nil {
			continue
		}
		m := map[string]addr{}
		for cid, ip := range n.IPs {
			m[cid] = addr{ip, n.Gateway, sub.Bits()}
		}
		byNet[n.Name] = m
	}
	e.net.mu.Unlock()

	e.ctr.mu.Lock()
	defer e.ctr.mu.Unlock()
	dirty := false
	for _, c := range e.ctr.byID {
		for name, ep := range c.Networks {
			a, ok := byNet[name][c.ID]
			if !ok || (ep.IPAddress == a.ip && ep.Gateway == a.gateway && ep.IPPrefixLen == a.bits) {
				continue
			}
			ep.IPAddress, ep.Gateway, ep.IPPrefixLen = a.ip, a.gateway, a.bits
			ep.MacAddress = containerMAC(c.ID)
			c.Networks[name] = ep
			dirty = true
		}
	}
	if dirty {
		if err := e.saveContainersLocked(); err != nil {
			e.logf("engine: save containers.json: %v", err)
		}
	}
}
