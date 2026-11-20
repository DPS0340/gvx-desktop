package recipes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestImageRegistry(t *testing.T) {
	r := ImageRegistry()
	if r.Ports[0] != "127.0.0.1:5001:5000" || r.Volumes()[0] != "gvx-registry" || r.Recipe != Registry {
		t.Errorf("registry %+v", r)
	}
	if addr, ok := HostAddress(r, 5000); !ok || addr != "127.0.0.1:5001" {
		t.Errorf("reached at %q", addr)
	}
	if got := DockerEngine().Command; got != "--tls=false --insecure-registry=host.docker.internal:5001" {
		t.Errorf("dockerd %q", got)
	}
	if got := KubernetesNode().Command; !strings.HasSuffix(got, " --private-registry=/output/registries.yaml") {
		t.Errorf("k3s %q", got)
	}
	want := `# Written by GVX Desktop at each start of the cluster: the image registry
# recipe, as the cluster's nodes reach it.
mirrors:
  "localhost:5001":
    endpoint:
      - "http://host.docker.internal:5001"
  "host.docker.internal:5001":
    endpoint:
      - "http://host.docker.internal:5001"
`
	if got := RegistriesYAML(); got != want {
		t.Errorf("registries.yaml\n%s", got)
	}
}

// A sandbox saved before the registry is brought to it, once, and keeps
// everything else of it.
func TestUpgrade(t *testing.T) {
	old := DockerEngine()
	old.Command = "--tls=false --debug"
	old.Memory = "4g"
	up, changed := Upgrade(old)
	if !changed || up.Command != "--tls=false --debug --insecure-registry=host.docker.internal:5001" || up.Memory != "4g" {
		t.Errorf("engine: %v %q", changed, up.Command)
	}
	if old.Command != "--tls=false --debug" {
		t.Error("the sandbox given was changed")
	}
	if _, changed := Upgrade(up); changed {
		t.Error("upgraded twice")
	}
	if _, changed := Upgrade(DockerEngine()); changed {
		t.Error("a new engine was upgraded")
	}
	before := DockerEngine()
	before.ExtraArgs = "--tune a=b"
	if up, changed := Upgrade(before); !changed || up.ExtraArgs != "--tune a=b -auto-publish" || up.Command != before.Command {
		t.Errorf("auto-publish: %v %q", changed, up.ExtraArgs)
	}
	before.ExtraArgs = "-auto-publish=false"
	if _, changed := Upgrade(before); changed {
		t.Error("auto-publish turned off by hand was turned on")
	}
	two := old
	two.Command = "--insecure-registry host.docker.internal:5001"
	if _, changed := Upgrade(two); changed {
		t.Error("the flag in two words was not seen")
	}

	// Old unix and TCP API publishes migrate to the app-private backend socket,
	// keeping every other port and setting untouched, and upgrading again is a no-op.
	for _, tc := range []struct {
		name     string
		port     string
		wantTCP  bool
		wantPort int
	}{
		{"old unix", "unix:~/.gvx/docker.sock:2375", false, DefaultDockerPort},
		{"old unix with whitespace", "  unix:~/.gvx/docker.sock:2375  ", false, DefaultDockerPort},
		{"old tcp ip:host:guest", "127.0.0.1:2375:2375", true, 2375},
		{"old tcp custom port", "127.0.0.1:23750:2375", true, 23750},
		{"old tcp with /tcp and whitespace", "  127.0.0.1:23750:2375/tcp  ", true, 23750},
		{"host:guest", "23750:2375", true, 23750},
		{"host:guest/tcp", "23750:2375/tcp", true, 23750},
		{"guest only", "2375", true, 2375},
		{"guest only /tcp", "2375/tcp", true, 2375},
		{"ip:guest", "127.0.0.1:2375", true, 2375},
		{"ipv6 ip:guest", "[::1]:2375", true, 2375},
		{"ipv6 ip:host:guest", "[::1]:23751:2375", true, 23751},
	} {
		orig := DockerEngine()
		orig.Memory = "3g"
		orig.Ports = []string{"127.0.0.1:8080:80", tc.port, "127.0.0.1:9000-9019:9000-9019"}
		api, ok := LegacyDockerAPI(orig)
		if !ok || api.TCP != tc.wantTCP || api.Port != tc.wantPort {
			t.Errorf("%s: LegacyDockerAPI = (%+v, %v), want (TCP=%v, Port=%d)", tc.name, api, ok, tc.wantTCP, tc.wantPort)
		}
		up, changed := Upgrade(orig)
		if !changed {
			t.Errorf("%s: not upgraded", tc.name)
			continue
		}
		if len(up.Ports) != 3 || up.Ports[0] != "127.0.0.1:8080:80" || up.Ports[1] != DockerBackendPublish || up.Ports[2] != "127.0.0.1:9000-9019:9000-9019" {
			t.Errorf("%s: ports = %v", tc.name, up.Ports)
		}
		if up.Memory != "3g" {
			t.Errorf("%s: memory changed to %q", tc.name, up.Memory)
		}
		if orig.Ports[1] != tc.port {
			t.Errorf("%s: caller spec was mutated: %v", tc.name, orig.Ports)
		}
		if _, again := Upgrade(up); again {
			t.Errorf("%s: upgraded twice", tc.name)
		}
	}

	// Non-API ports (UDP, ranges containing 2375) are not misidentified, and a
	// Docker Engine spec missing any API publish has DockerBackendPublish prepended.
	for _, nonAPI := range []string{"127.0.0.1:2375:2375/udp", "2375/udp", "127.0.0.1:2370-2380:2370-2380"} {
		spec := DockerEngine()
		spec.Ports = []string{nonAPI, "127.0.0.1:9000-9019:9000-9019"}
		if api, ok := LegacyDockerAPI(spec); ok {
			t.Errorf("%s: unexpectedly recognized as Docker API: %+v", nonAPI, api)
		}
		up, changed := Upgrade(spec)
		if !changed || len(up.Ports) != 3 || up.Ports[0] != DockerBackendPublish || up.Ports[1] != nonAPI {
			t.Errorf("%s: missing API port upgrade = (%v, %v)", nonAPI, changed, up.Ports)
		}
		if _, again := Upgrade(up); again {
			t.Errorf("%s: upgraded twice", nonAPI)
		}
	}

	k3s := WithServiceLB(KubernetesNode(), true)
	k3s.Command = "server --disable=traefik --write-kubeconfig=/output/kubeconfig.yaml --write-kubeconfig-mode=644"
	up, changed = Upgrade(k3s)
	if !changed || up.Command != k3s.Command+" --private-registry=/output/registries.yaml" || !ServiceLBOf(up) || len(up.Ports) != 4 {
		t.Errorf("cluster: %v %q", changed, up.Command)
	}
	if _, changed := Upgrade(up); changed {
		t.Error("upgraded twice")
	}
	mine := k3s
	mine.Command += " --private-registry=/etc/mine.yaml"
	if _, changed := Upgrade(mine); changed {
		t.Error("the user's own registries.yaml was replaced")
	}
	noOutput := k3s.Clone()
	noOutput.Mounts = noOutput.Mounts[:1]
	if _, changed := Upgrade(noOutput); changed {
		t.Error("a cluster without the /output folder was upgraded")
	}
	if _, changed := Upgrade(sandbox.Spec{Command: "sh", Image: "alpine"}); changed {
		t.Error("a sandbox of no recipe was upgraded")
	}
}

// The cluster's registries.yaml is written into the folder of this
// computer that /output is, and only for the file the recipe names.
func TestPrepare(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "k3s")
	k3s := KubernetesNode()
	k3s.Mounts[1].Source = dir
	if err := Prepare(k3s); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "registries.yaml"))
	if err != nil || string(got) != RegistriesYAML() {
		t.Fatalf("written %q, %v", got, err)
	}
	// Written again as the recipe has it, over what was changed.
	os.WriteFile(filepath.Join(dir, "registries.yaml"), []byte("x"), 0o644)
	Prepare(k3s)
	if got, _ := os.ReadFile(filepath.Join(dir, "registries.yaml")); string(got) != RegistriesYAML() {
		t.Errorf("not written again: %q", got)
	}

	other := filepath.Join(t.TempDir(), "other")
	mine := k3s.Clone()
	mine.Mounts[1].Source = other
	mine.Command = "server --private-registry=/etc/rancher/k3s/registries.yaml"
	Prepare(mine)
	old := mine.Clone()
	old.Command = "server"
	Prepare(old)
	engine := DockerEngine()
	engine.Mounts = append(engine.Mounts, sandbox.Mount{Type: sandbox.MountBind, Source: other, Target: "/output"})
	Prepare(engine)
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("a file was written where none is asked for: %v", err)
	}
}
