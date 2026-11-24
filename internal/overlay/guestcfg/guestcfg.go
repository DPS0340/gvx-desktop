// Package guestcfg is what the engine tells netinit, the program that
// starts a container on a user network or with extra hosts: the WireGuard
// link that joins the container to the app's hub, the lines it adds to
// /etc/hosts, and the user the container's command runs as.
// The engine encodes a Config into one environment variable; netinit
// decodes it, removes the variable, sets the link up, and runs the command.
package guestcfg

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// EnvVar is the environment variable that carries the Config.
const EnvVar = "GVX_NET"

// MountDir is the read-only directory where the engine mounts netinit in the
// container. Mounting a directory rather than a single file lets rungv's
// hostfs load .gvx_meta on Windows, where NTFS carries no POSIX execute bit.
const MountDir = "/.gvx-netinit.d"

// BinaryName is the netinit executable file name inside MountDir.
const BinaryName = "netinit"

// Path is the netinit executable path inside the container.
const Path = MountDir + "/" + BinaryName

// ApplyArg is argv[1] for netinit's live reconcile mode (docker network
// connect/disconnect on a running container via rungv exec).
const ApplyArg = "--gvx-apply"

// LinkName is the name of the link netinit makes.
const LinkName = "gvx0"

// Config is the guest's side of the overlay.
type Config struct {
	// PrivateKey and HubPublicKey are WireGuard keys, 32 bytes each.
	PrivateKey   []byte `json:"key"`
	HubPublicKey []byte `json:"hub"`
	// Endpoint is the hub as the guest reaches it, such as 10.0.2.2:51820.
	Endpoint string `json:"endpoint"`
	// Addresses are the container's addresses on its networks, as CIDR
	// prefixes: 10.89.1.5/24.
	Addresses []string `json:"addrs"`
	// Routes are more prefixes the link carries, besides the addresses'.
	Routes []string `json:"routes,omitempty"`
	MTU    int      `json:"mtu,omitempty"`
	// Keepalive is the persistent keepalive in seconds, which keeps the
	// sandbox's NAT entry for the hub open.
	Keepalive int `json:"keepalive,omitempty"`
	// User is the container's user, as docker run -u takes it; "" runs the
	// command as root.
	User string `json:"user,omitempty"`
	// KeepHome keeps HOME as it is when netinit drops to User: the
	// container set it. Otherwise HOME becomes the user's home.
	KeepHome bool `json:"keepHome,omitempty"`
	// Hosts are lines for /etc/hosts, "IP NAME" each: docker run
	// --add-host and Compose extra_hosts.
	Hosts []string `json:"hosts,omitempty"`
	// Isolated takes the sandbox's default route away, as a container
	// only on internal networks has no way out. The routes to the hub and
	// the DNS relay stay.
	Isolated bool `json:"isolated,omitempty"`
}

// HasLink reports whether c makes a WireGuard link.
func (c Config) HasLink() bool { return len(c.Addresses) > 0 }

// Encode returns c as the value of EnvVar.
func (c Config) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Decode reads a value of EnvVar.
func Decode(s string) (Config, error) {
	var c Config
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return c, fmt.Errorf("decode %s: %w", EnvVar, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("decode %s: %w", EnvVar, err)
	}
	if c.HasLink() && (len(c.PrivateKey) != 32 || len(c.HubPublicKey) != 32) {
		return c, errors.New("decode " + EnvVar + ": keys must be 32 bytes")
	}
	return c, nil
}

// Identity is who a command runs as.
type Identity struct {
	UID, GID uint32
	Groups   []uint32 // supplementary groups
	Home     string
}

// ResolveUser resolves a docker user spec (user, uid, user:group, uid:gid)
// against the guest's /etc/passwd and /etc/group contents, as docker does:
// a name must exist; a uid that has no entry runs with gid 0 (or the given
// group) and home /.
func ResolveUser(spec, passwd, group string) (Identity, error) {
	userPart, groupPart, hasGroup := strings.Cut(strings.TrimSpace(spec), ":")
	id := Identity{Home: "/"}
	var userName string

	users := parsePasswd(passwd)
	if n, err := strconv.ParseUint(userPart, 10, 32); err == nil {
		id.UID = uint32(n)
		for _, u := range users {
			if u.uid == id.UID {
				id.GID, id.Home, userName = u.gid, u.home, u.name
				break
			}
		}
	} else {
		found := false
		for _, u := range users {
			if u.name == userPart {
				id.UID, id.GID, id.Home, userName = u.uid, u.gid, u.home, u.name
				found = true
				break
			}
		}
		if !found {
			return Identity{}, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
	}

	groups := parseGroup(group)
	if hasGroup && groupPart != "" {
		if n, err := strconv.ParseUint(groupPart, 10, 32); err == nil {
			id.GID = uint32(n)
		} else {
			found := false
			for _, g := range groups {
				if g.name == groupPart {
					id.GID = g.gid
					found = true
					break
				}
			}
			if !found {
				return Identity{}, fmt.Errorf("unable to find group %s: no matching entries in group file", groupPart)
			}
		}
	}
	if userName != "" {
		for _, g := range groups {
			for _, m := range g.members {
				if m == userName && g.gid != id.GID {
					id.Groups = append(id.Groups, g.gid)
				}
			}
		}
	}
	if id.Home == "" {
		id.Home = "/"
	}
	return id, nil
}

type passwdEntry struct {
	name     string
	uid, gid uint32
	home     string
}

func parsePasswd(s string) []passwdEntry {
	var out []passwdEntry
	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 6 || strings.HasPrefix(f[0], "#") {
			continue
		}
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		gid, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, passwdEntry{name: f[0], uid: uint32(uid), gid: uint32(gid), home: f[5]})
	}
	return out
}

type groupEntry struct {
	name    string
	gid     uint32
	members []string
}

func parseGroup(s string) []groupEntry {
	var out []groupEntry
	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 3 || strings.HasPrefix(f[0], "#") {
			continue
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		g := groupEntry{name: f[0], gid: uint32(gid)}
		if len(f) >= 4 && f[3] != "" {
			for m := range strings.SplitSeq(f[3], ",") {
				if m = strings.TrimSpace(m); m != "" {
					g.members = append(g.members, m)
				}
			}
		}
		out = append(out, g)
	}
	return out
}

// ParseExtraHost reads a docker extra host, "NAME:IP" or "NAME=IP", where
// the IP may be IPv6 and the word host-gateway stands for gateway. It
// returns the /etc/hosts line, "IP NAME".
func ParseExtraHost(s, gateway string) (string, error) {
	name, ip, ok := strings.Cut(s, "=")
	if !ok {
		name, ip, ok = strings.Cut(s, ":")
	}
	name = strings.TrimSpace(name)
	ip = strings.Trim(strings.TrimSpace(ip), "[]")
	if !ok || name == "" || strings.ContainsAny(name, " \t#") {
		return "", fmt.Errorf("invalid extra host %q: want NAME:IP", s)
	}
	if ip == "host-gateway" {
		ip = gateway
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("invalid IP address in extra host %q", s)
	}
	return a.String() + "\t" + name, nil
}

// DiffPrefixes returns the addresses to add to and delete from a link so
// its addresses match want. Both slices are deduplicated and sorted.
func DiffPrefixes(current, want []netip.Prefix) (add, del []netip.Prefix) {
	curSet := make(map[netip.Prefix]bool, len(current))
	for _, p := range current {
		if p.IsValid() {
			curSet[p] = true
		}
	}
	wantSet := make(map[netip.Prefix]bool, len(want))
	for _, p := range want {
		if p.IsValid() {
			wantSet[p] = true
		}
	}
	for p := range wantSet {
		if !curSet[p] {
			add = append(add, p)
		}
	}
	for p := range curSet {
		if !wantSet[p] {
			del = append(del, p)
		}
	}
	cmpPrefix := func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Bits(), b.Bits())
	}
	slices.SortFunc(add, cmpPrefix)
	slices.SortFunc(del, cmpPrefix)
	return add, del
}

// UncoveredRoutes returns the masked routes from routes that are not
// already implied by any interface address in addrs.
func UncoveredRoutes(addrs, routes []netip.Prefix) []netip.Prefix {
	covered := make(map[netip.Prefix]bool, len(addrs))
	for _, a := range addrs {
		if a.IsValid() {
			covered[a.Masked()] = true
		}
	}
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	for _, r := range routes {
		if !r.IsValid() {
			continue
		}
		m := r.Masked()
		if covered[m] || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Bits(), b.Bits())
	})
	return out
}

// AllowedIPs returns the sorted, deduplicated set of masked prefixes the
// WireGuard hub peer should allow for addrs and routes.
func AllowedIPs(addrs, routes []netip.Prefix) []netip.Prefix {
	seen := make(map[netip.Prefix]bool, len(addrs)+len(routes))
	var out []netip.Prefix
	for _, p := range addrs {
		if !p.IsValid() {
			continue
		}
		m := p.Masked()
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, p := range routes {
		if !p.IsValid() {
			continue
		}
		m := p.Masked()
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Bits(), b.Bits())
	})
	return out
}
