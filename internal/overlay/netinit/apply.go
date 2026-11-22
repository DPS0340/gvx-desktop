//go:build linux

package main

import (
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

// applyLink reconciles gvx0 with cfg for live network connect/disconnect:
// it creates gvx0 when missing, updates WireGuard peer allowed IPs, adds
// missing addresses, removes extra ones, reconciles RTPROT_BOOT routes, and
// brings gvx0 down with no routes when disconnected from all user networks.
// It does not touch /etc/hosts, isolation routes, or the process user.
func applyLink(cfg guestcfg.Config) error {
	iface, err := net.InterfaceByName(guestcfg.LinkName)
	if err != nil {
		if !cfg.HasLink() {
			return nil
		}
		return setupLink(cfg)
	}
	endpoint, addrs, routes, _, err := parseLinkConfig(cfg)
	if err != nil {
		return err
	}

	rawAddrs, err := iface.Addrs()
	if err != nil {
		return fmt.Errorf("read %s addresses: %w", guestcfg.LinkName, err)
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

	index := uint32(iface.Index)
	if len(cfg.PrivateKey) == 32 && len(cfg.HubPublicKey) == 32 {
		if err := configureWireGuard(index, cfg, endpoint, addrs, routes); err != nil {
			return err
		}
	}

	rt, err := dial(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer rt.close()

	curRoutes, err := listBootRoutes(rt, index)
	if err != nil {
		return err
	}

	toAdd, toDel := guestcfg.DiffPrefixes(current, addrs)
	for _, p := range toDel {
		if err := delAddr(rt, index, p); err != nil {
			return err
		}
	}
	for _, p := range toAdd {
		if err := addAddr(rt, index, p); err != nil {
			return err
		}
	}

	if len(addrs) == 0 {
		for _, p := range curRoutes {
			if err := delRoute(rt, p, index); err != nil {
				return err
			}
		}
		if err := setLinkDown(rt, index); err != nil {
			return err
		}
		return nil
	}

	if err := setLinkUp(rt, index); err != nil {
		return err
	}
	wantRoutes := guestcfg.UncoveredRoutes(addrs, routes)
	addRoutes, delRoutes := guestcfg.DiffPrefixes(curRoutes, wantRoutes)
	for _, p := range delRoutes {
		if err := delRoute(rt, p, index); err != nil {
			return err
		}
	}
	for _, p := range addRoutes {
		if err := addRoute(rt, p, index); err != nil {
			return err
		}
	}
	return nil
}
