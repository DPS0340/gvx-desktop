//go:build !windows

package engine

import (
	"strings"
	"testing"
)

// An image whose USER is not root starts on a user network, where netinit
// sets up the network as root and then drops to that user. Started as the
// image's USER instead, netinit failed with "setgroups: operation not
// permitted" and the container exited with 126.
func TestImageUserOnUserNetworkHarness(t *testing.T) {
	h := startHarness(t)
	if _, errOut, code := h.docker(t, "", "pull", "alpine:3.20"); code != 0 {
		t.Fatalf("pull alpine:3.20 (%d): %s", code, errOut)
	}
	t.Cleanup(func() {
		_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-nonroot-src")
		_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-nonroot:1")
		_, _, _ = h.docker(t, "", "network", "rm", "gvxd-nonroot-net")
	})
	if _, errOut, code := h.docker(t, "", "create", "--name", "gvxd-nonroot-src", "alpine:3.20", "true"); code != 0 {
		t.Fatalf("create: %s", errOut)
	}
	if _, errOut, code := h.docker(t, "", "commit", "--change", "USER nobody", "gvxd-nonroot-src", "gvxd-nonroot:1"); code != 0 {
		t.Fatalf("commit: %s", errOut)
	}
	if _, errOut, code := h.docker(t, "", "network", "create", "gvxd-nonroot-net"); code != 0 {
		t.Fatalf("network create: %s", errOut)
	}

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"the image's user", nil, "65534"},
		{"a user given to run", []string{"--user", "root"}, "0"},
	} {
		args := append([]string{"run", "--rm", "--network", "gvxd-nonroot-net"}, c.args...)
		args = append(args, "gvxd-nonroot:1", "id", "-u")
		out, errOut, code := h.docker(t, "", args...)
		if code != 0 || strings.TrimSpace(out) != c.want {
			t.Errorf("%s: id -u = %q (exit %d), want %s: %s", c.name, strings.TrimSpace(out), code, c.want, errOut)
		}
	}
}
