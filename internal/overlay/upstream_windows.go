//go:build windows

package overlay

import (
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hostDNSServers returns the DNS servers of the adapters that are up.
func hostDNSServers() []string {
	size := uint32(15 << 10)
	var (
		buf []byte
		err error
	)
	for range 4 {
		if size == 0 {
			return nil
		}
		buf = make([]byte, size)
		err = windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil
		}
	}
	if err != nil || size == 0 || len(buf) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		if aa.OperStatus != windows.IfOperStatusUp {
			continue
		}
		for dns := aa.FirstDnsServerAddress; dns != nil; dns = dns.Next {
			ip := dns.Address.IP()
			a, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			a = a.Unmap()
			if !a.IsValid() || a.IsUnspecified() {
				continue
			}
			if a.Is6() && (a.IsLinkLocalUnicast() || a.String() == "fec0:0:0:ffff::1" || a.String() == "fec0:0:0:ffff::2" || a.String() == "fec0:0:0:ffff::3") {
				continue
			}
			s := net.JoinHostPort(a.String(), "53")
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
