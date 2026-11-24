//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An image that ships /etc/resolv.conf (Debian's base image names 1.1.1.1)
// keeps it under rungv, which writes its own only when the image has none.
// On a user network such a container asked past rungv's DNS relay, so the
// other containers' names did not resolve. netinit now makes the file name
// the relay, on a read-only root too, and a resolv.conf the container mounts
// stays as it is.
func TestImageResolvConfOnUserNetworkHarness(t *testing.T) {
	h := startHarness(t)
	if _, errOut, code := h.docker(t, "", "pull", "alpine:3.20"); code != 0 {
		t.Fatalf("pull alpine:3.20 (%d): %s", code, errOut)
	}
	t.Cleanup(func() {
		_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-resolv-srv")
		_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-resolv:1")
		_, _, _ = h.docker(t, "", "network", "rm", "gvxd-resolv-net")
	})

	ctxDir := t.TempDir()
	files := map[string]string{
		"Dockerfile":  "FROM alpine:3.20\nCOPY resolv.conf /etc/resolv.conf\n",
		"resolv.conf": "nameserver 1.1.1.1\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, errOut, code := h.docker(t, "", "buildx", "create", "--name", BuilderName, "--driver", "remote",
		"--driver-opt", "default-load=true", "unix://"+h.e.BuilderSocket()); code != 0 {
		t.Fatalf("buildx create (%d): %s", code, errOut)
	}
	if _, errOut, code := h.docker(t, "", "buildx", "build", "--builder", BuilderName, "-t", "gvxd-resolv:1", ctxDir); code != 0 {
		t.Fatalf("build (%d): %s", code, errOut)
	}
	if out, errOut, code := h.docker(t, "", "run", "--rm", "gvxd-resolv:1", "cat", "/etc/resolv.conf"); code != 0 || strings.TrimSpace(out) != "nameserver 1.1.1.1" {
		t.Fatalf("the image's resolv.conf on the bridge = %q (exit %d): %s", out, code, errOut)
	}

	if _, errOut, code := h.docker(t, "", "network", "create", "gvxd-resolv-net"); code != 0 {
		t.Fatalf("network create: %s", errOut)
	}
	if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-resolv-srv", "--network", "gvxd-resolv-net", "alpine:3.20", "sleep", "300"); code != 0 {
		t.Fatalf("run server: %s", errOut)
	}
	ipOut, errOut, code := h.docker(t, "", "inspect", "-f", `{{(index .NetworkSettings.Networks "gvxd-resolv-net").IPAddress}}`, "gvxd-resolv-srv")
	srvIP := strings.TrimSpace(ipOut)
	if code != 0 || srvIP == "" {
		t.Fatalf("server address = %q (exit %d): %s", srvIP, code, errOut)
	}

	out, errOut, code := h.docker(t, "", "run", "--rm", "--network", "gvxd-resolv-net", "gvxd-resolv:1",
		"sh", "-c", "cat /etc/resolv.conf && getent hosts gvxd-resolv-srv")
	if code != 0 || !strings.Contains(out, "nameserver 10.0.2.3") || strings.Contains(out, "1.1.1.1") || !strings.Contains(out, srvIP) {
		t.Errorf("on the user network: %q (exit %d), want the relay and %s: %s", out, code, srvIP, errOut)
	}

	// A read-only root cannot take a new file, so netinit binds one over it.
	out, errOut, code = h.docker(t, "", "run", "--rm", "--read-only", "--network", "gvxd-resolv-net", "gvxd-resolv:1",
		"sh", "-c", "! touch /etc/gvxd-probe 2>/dev/null && cat /etc/resolv.conf && getent hosts gvxd-resolv-srv")
	if code != 0 || !strings.Contains(out, "nameserver 10.0.2.3") || strings.Contains(out, "1.1.1.1") || !strings.Contains(out, srvIP) {
		t.Errorf("with a read-only root: %q (exit %d), want the relay and %s: %s", out, code, srvIP, errOut)
	}

	own := filepath.Join(t.TempDir(), "resolv.conf")
	const ownConf = "nameserver 192.0.2.1\n"
	if err := os.WriteFile(own, []byte(ownConf), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = h.docker(t, "", "run", "--rm", "--network", "gvxd-resolv-net", "-v", own+":/etc/resolv.conf",
		"gvxd-resolv:1", "cat", "/etc/resolv.conf")
	if code != 0 || out != ownConf {
		t.Errorf("a mounted resolv.conf = %q (exit %d), want %q: %s", out, code, ownConf, errOut)
	}
	if b, err := os.ReadFile(own); err != nil || string(b) != ownConf {
		t.Errorf("the host's file = %q, %v; want it untouched", b, err)
	}
}
