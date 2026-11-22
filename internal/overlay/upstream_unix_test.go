//go:build !windows

package overlay

import (
	"slices"
	"testing"
)

func TestParseResolvConf(t *testing.T) {
	got := parseResolvConf("# x\nnameserver 192.168.1.1\nnameserver fe80::1%en0\nnameserver 2001:db8::53\nsearch lan\nnameserver bogus\n")
	if !slices.Equal(got, []string{"192.168.1.1:53", "[2001:db8::53]:53"}) {
		t.Fatalf("parseResolvConf = %v", got)
	}
}
