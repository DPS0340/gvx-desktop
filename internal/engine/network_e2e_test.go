//go:build !windows

package engine

// Real docker CLI and Compose over real sandboxes: containers on a user
// network reach each other by name, and containers on other networks do
// not. Skipped unless GVX_ENGINE_HARNESS=1 and without -short.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// server answers HTTP on 8080 with its name, as the user nobody.
const e2eServer = `while true; do printf "HTTP/1.0 200 OK\r\n\r\nhello from $(id -un)\n" | nc -l -p 8080; done`

func TestHarnessNetworks(t *testing.T) {
	h := startHarness(t)
	if _, errOut, code := h.docker(t, "", "pull", "alpine:3.20"); code != 0 {
		t.Fatalf("pull: %s", errOut)
	}

	t.Run("docker_run", func(t *testing.T) {
		if _, errOut, code := h.docker(t, "", "network", "create", "gvxd-e2e-net"); code != 0 {
			t.Fatalf("network create: %s", errOut)
		}
		t.Cleanup(func() {
			h.docker(t, "", "rm", "-f", "gvxd-e2e-srv")
			h.docker(t, "", "network", "rm", "gvxd-e2e-net")
		})
		if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-e2e-srv", "--network", "gvxd-e2e-net",
			"--network-alias", "api", "-u", "nobody", "alpine:3.20", "sh", "-c", e2eServer); code != 0 {
			t.Fatalf("run server: %s", errOut)
		}
		for _, name := range []string{"gvxd-e2e-srv", "api"} {
			out, errOut, code := h.docker(t, "", "run", "--rm", "--network", "gvxd-e2e-net", "alpine:3.20", "sh", "-c",
				"for i in $(seq 1 20); do wget -qO- -T 2 http://"+name+":8080/ && exit 0; sleep 0.5; done; exit 1")
			if code != 0 || !strings.Contains(out, "hello from nobody") {
				t.Fatalf("reach %s (%d): out=%q err=%q", name, code, out, errOut)
			}
		}
		// Other names go to the host's resolvers, and the internet and the
		// host stay reachable through the sandbox's NAT.
		out, errOut, code := h.docker(t, "", "run", "--rm", "--network", "gvxd-e2e-net", "alpine:3.20", "sh", "-c",
			"nslookup registry-1.docker.io >/dev/null && grep -q host.docker.internal /etc/hosts && ip route | grep -q '^default via 10.0.2.2 dev eth0' && echo ok")
		if code != 0 || !strings.Contains(out, "ok") {
			t.Fatalf("egress (%d): out=%q err=%q", code, out, errOut)
		}
		// The default bridge has no names and no route to the network.
		out, _, code = h.docker(t, "", "run", "--rm", "alpine:3.20", "sh", "-c", "wget -qO- -T 2 http://gvxd-e2e-srv:8080/")
		if code == 0 {
			t.Fatalf("a container on the default bridge reached the network: %q", out)
		}
		out, errOut, code = h.docker(t, "", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "gvxd-e2e-srv")
		if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "10.89.") {
			t.Fatalf("inspect address (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("compose", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "compose.yaml")
		yaml := `services:
  web:
    image: alpine:3.20
    user: nobody
    command: ["sh", "-c", ` + "`" + e2eServer + "`" + `]
  client:
    image: alpine:3.20
    depends_on: [web]
    command: ["sh", "-c", "for i in $$(seq 1 20); do wget -qO- -T 2 http://web:8080/ && exit 0; sleep 0.5; done; exit 1"]
  outsider:
    image: alpine:3.20
    networks: [other]
    depends_on: [web]
    command: ["sh", "-c", "sleep 2; wget -qO- -T 2 http://web:8080/ && exit 0; exit 7"]
networks:
  other: {}
`
		yaml = strings.ReplaceAll(yaml, "`", `'`)
		if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		compose := func(args ...string) (string, string, int) {
			return h.docker(t, "", append([]string{"compose", "-p", "gvxd-e2e", "-f", file}, args...)...)
		}
		t.Cleanup(func() { compose("down", "-v", "-t", "0") })
		if _, errOut, code := compose("up", "-d"); code != 0 {
			t.Fatalf("compose up: %s", errOut)
		}
		for svc, want := range map[string]string{"client": "0", "outsider": "7"} {
			out, errOut, code := h.docker(t, "", "wait", "gvxd-e2e-"+svc+"-1")
			if code != 0 || strings.TrimSpace(out) != want {
				logs, _, _ := compose("logs", svc)
				t.Fatalf("%s exited %q (%d, %s), want %s; logs:\n%s", svc, out, code, errOut, want, logs)
			}
		}
		logs, _, _ := compose("logs", "client")
		if !strings.Contains(logs, "hello from nobody") {
			t.Fatalf("client logs:\n%s", logs)
		}
	})

	t.Run("extra_hosts", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "run", "--rm", "--add-host", "db:10.0.0.5", "--add-host", "gw=host-gateway",
			"-u", "nobody", "alpine:3.20", "sh", "-c", "getent hosts db gw; id -un")
		if code != 0 || !strings.Contains(out, "10.0.0.5") || !strings.Contains(out, "10.0.2.2") || !strings.Contains(out, "nobody") {
			t.Fatalf("extra hosts (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("internal", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "compose.yaml")
		yaml := `services:
  db:
    image: alpine:3.20
    networks: [secret]
    command: ["sh", "-c", 'while true; do printf "HTTP/1.0 200 OK\r\n\r\nsecret\n" | nc -l -p 8080; done']
  app:
    image: alpine:3.20
    networks: [secret]
    depends_on: [db]
    command: ["sh", "-c", "for i in $$(seq 1 20); do wget -qO- -T 2 http://db:8080/ && break; sleep 0.5; done || exit 1; if nslookup example.com >/dev/null 2>&1; then exit 2; fi; if wget -qO- -T 3 http://1.1.1.1/ >/dev/null 2>&1; then exit 3; fi; exit 0"]
networks:
  secret:
    internal: true
`
		if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		compose := func(args ...string) (string, string, int) {
			return h.docker(t, "", append([]string{"compose", "-p", "gvxd-e2e-int", "-f", file}, args...)...)
		}
		t.Cleanup(func() { compose("down", "-v", "-t", "0") })
		if _, errOut, code := compose("up", "-d"); code != 0 {
			t.Fatalf("compose up: %s", errOut)
		}
		out, errOut, code := h.docker(t, "", "wait", "gvxd-e2e-int-app-1")
		if code != 0 || strings.TrimSpace(out) != "0" {
			logs, _, _ := compose("logs", "app")
			t.Fatalf("app exited %q (%d, %s), want 0 (1: db unreachable, 2: external DNS answered, 3: internet reached); logs:\n%s", out, code, errOut, logs)
		}
	})
}
