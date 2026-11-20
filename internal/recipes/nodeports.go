package recipes

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// The NodePorts of a cluster are 30000-32767 unless its API server is told
// otherwise. A NodePort service takes one on every node, and the k3s
// recipe's node is the sandbox: a NodePort reaches this computer only when
// the sandbox publishes that port of its own.
const (
	NodePortMin = 30000
	NodePortMax = 32767
)

// tcpRange is a run of TCP ports a sandbox publishes: guest port Guest+i
// is reached at port Host+i of the address IP, for i below Len.
type tcpRange struct {
	IP          string
	Host, Guest int
	Len         int
}

// publishedTCP reads the TCP ports a sandbox spec publishes, in the -p
// forms rungv takes: [IP:][HOST:]GUEST[/tcp], each of HOST and GUEST a
// port or a range. Sockets, UDP and what does not read are left out.
func publishedTCP(spec sandbox.Spec) []tcpRange {
	var out []tcpRange
	for _, p := range spec.Ports {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "unix:") || sandbox.ValidatePort(p) != nil {
			continue
		}
		if rest, proto, ok := strings.Cut(p, "/"); ok {
			if proto != "tcp" {
				continue
			}
			p = rest
		}
		ip := ""
		if strings.HasPrefix(p, "[") {
			end := strings.Index(p, "]:")
			if end < 0 {
				continue
			}
			ip, p = p[:end+1], p[end+2:]
		}
		parts := strings.Split(p, ":")
		if len(parts) == 3 {
			ip, parts = parts[0], parts[1:]
		} else if len(parts) == 2 && strings.Contains(parts[0], ".") {
			ip, parts = parts[0], parts[1:]
		}
		guestLo, n, ok := portRange(parts[len(parts)-1])
		if !ok {
			continue
		}
		hostLo := guestLo
		if len(parts) == 2 {
			if hostLo, _, ok = portRange(parts[0]); !ok {
				continue
			}
		}
		out = append(out, tcpRange{IP: ip, Host: hostLo, Guest: guestLo, Len: n})
	}
	return out
}

// portRange reads "30000" or "30000-30019" as its first port and length.
func portRange(s string) (lo, n int, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	lo, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	if !isRange {
		return lo, 1, true
	}
	hi, err := strconv.Atoi(b)
	if err != nil || hi < lo {
		return 0, 0, false
	}
	return lo, hi - lo + 1, true
}

// HostAddress is the host:port of this computer that reaches a TCP port of
// the sandbox, through the ports it publishes, or false when none carries
// it. A port published on every address is reached on the loopback one.
// For the k3s recipe, whose node is the sandbox, the port is a NodePort,
// or one ServiceLB takes for a LoadBalancer service.
func HostAddress(spec sandbox.Spec, guestPort int) (string, bool) {
	for _, r := range publishedTCP(spec) {
		if guestPort < r.Guest || guestPort >= r.Guest+r.Len {
			continue
		}
		ip := r.IP
		if ip == "" || ip == "0.0.0.0" {
			ip = "127.0.0.1"
		} else if ip == "[::]" {
			ip = "[::1]"
		}
		return ip + ":" + strconv.Itoa(r.Host+guestPort-r.Guest), true
	}
	return "", false
}

// NodePortRanges are the NodePorts the sandbox publishes, written as
// 30000–30019, or 30080 alone.
func NodePortRanges(spec sandbox.Spec) []string {
	var out []string
	for _, r := range publishedTCP(spec) {
		lo, hi := max(r.Guest, NodePortMin), min(r.Guest+r.Len-1, NodePortMax)
		switch {
		case lo > hi:
			continue
		case lo == hi:
			out = append(out, strconv.Itoa(lo))
		default:
			out = append(out, fmt.Sprintf("%d–%d", lo, hi))
		}
	}
	return out
}
