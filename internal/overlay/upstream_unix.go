//go:build !windows

package overlay

import (
	"net"
	"net/netip"
	"os"
	"strings"
)

// hostDNSServers reads the nameservers of /etc/resolv.conf. The app runs
// on the host, so a loopback resolver such as systemd-resolved's works.
func hostDNSServers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	return parseResolvConf(string(b))
}

func parseResolvConf(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		host, _, _ := strings.Cut(f[1], "%") // fe80::1%en0
		a, err := netip.ParseAddr(host)
		if err != nil {
			continue
		}
		if a.Is6() && a.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, net.JoinHostPort(a.String(), "53"))
	}
	return out
}
