package recipes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestRecipesValidate(t *testing.T) {
	for _, r := range All() {
		if err := r.New().Validate(); err != nil {
			t.Errorf("%s: %v", r.Key, err)
		}
	}
}

// TestDockerEngine boots docker:dind in a sandbox and reaches it with the
// Docker API client, as the Docker page of the app does.
func TestDockerEngine(t *testing.T) {
	if testing.Short() || os.Getenv("GVXD_DOCKER_TEST") == "" {
		t.Skip("set GVXD_DOCKER_TEST=1 to boot docker:dind")
	}
	path, err := rungv.Locate(filepath.Join("..", "..", "bin", rungv.BinaryName()))
	if err != nil {
		t.Skip("rungv not found")
	}
	client := &rungv.Client{Path: path}
	sockDir, err := os.MkdirTemp("", "gvxd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "d.sock")

	spec := DockerEngine()
	spec.Ports = []string{"unix:" + sock + ":2375"}
	vol := "gvxd-docker-test"
	spec.Mounts[0].Source = vol
	defer client.RemoveVolume(context.Background(), vol)

	sup := &sandbox.Supervisor{Client: func() *rungv.Client { return client }, DataDir: t.TempDir()}
	defer sup.StopAll(20 * time.Second)
	if err := sup.Start(spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Minute)
	for sup.State(spec.ID).Status != sandbox.Running {
		st := sup.State(spec.ID)
		if !st.Status.Active() || time.Now().After(deadline) {
			for _, l := range sup.Logs(spec.ID).Snapshot() {
				t.Log(l.Text)
			}
			t.Fatalf("engine not ready: %+v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}
	api, err := dockerapi.New("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := api.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.KernelVersion, "gvisor") && v.OS != "linux" {
		t.Errorf("version %+v", v)
	}
	if _, err := api.Containers(ctx, true); err != nil {
		t.Fatal(err)
	}
	t.Logf("dockerd %s (API %s) on %s/%s kernel %s", v.Version, v.APIVersion, v.OS, v.Arch, v.KernelVersion)
}

// DockerAPI configures the app's proxy listener and DOCKER_HOST, while the
// engine sandbox always publishes dockerd on its app-private backend socket.
func TestDockerAPI(t *testing.T) {
	socket := DockerAPI{Port: DefaultDockerPort}
	tcp := DockerAPI{TCP: true, Port: 23750}
	if netw, addr := socket.ListenAddr(); netw != "unix" || addr != DockerSocket() {
		t.Errorf("socket listens on %q %q", netw, addr)
	}
	if netw, addr := tcp.ListenAddr(); netw != "tcp" || addr != "127.0.0.1:23750" {
		t.Errorf("tcp listens on %q %q", netw, addr)
	}
	if got := tcp.Host(); got != "tcp://127.0.0.1:23750" {
		t.Errorf("tcp host %q", got)
	}
	if got := (DockerAPI{TCP: true}).Host(); got != "tcp://127.0.0.1:2375" {
		t.Errorf("a port left out: %q", got)
	}
	if got := DockerBackendPublish; got != "unix:~/.gvx/engine/dockerd.sock:2375" {
		t.Errorf("backend publish %q", got)
	}
	spec := DockerEngine()
	if len(spec.Ports) != 2 || spec.Ports[0] != DockerBackendPublish || spec.Ports[1] != "127.0.0.1:9000-9019:9000-9019" {
		t.Errorf("engine ports %v", spec.Ports)
	}
}

// ServiceLB is off in a new cluster, turned on and off again without
// touching what is beside it, and read back from the command.
func TestServiceLB(t *testing.T) {
	k3s := KubernetesNode()
	if ServiceLBOf(k3s) || len(LoadBalancerPorts(k3s)) != 0 {
		t.Fatalf("a new cluster has ServiceLB: %q", k3s.Command)
	}
	if !strings.Contains(k3s.Command, "--disable=traefik,servicelb") {
		t.Errorf("command %q", k3s.Command)
	}
	on := WithServiceLB(k3s, true)
	if !ServiceLBOf(on) {
		t.Errorf("not on: %q", on.Command)
	}
	if !strings.Contains(on.Command, "--disable=traefik ") || strings.Contains(on.Command, "servicelb") {
		t.Errorf("traefik left on, or servicelb still named: %q", on.Command)
	}
	if got := strings.Join(on.Ports, " "); got != "127.0.0.1:6443:6443 127.0.0.1:30000-30019:30000-30019 127.0.0.1:8080:80 127.0.0.1:8443:443" {
		t.Errorf("ports %s", got)
	}
	if got := LoadBalancerPorts(on); len(got) != 2 || got[0] != 80 || got[1] != 443 {
		t.Errorf("load balancer ports %v", got)
	}
	if addr, ok := HostAddress(on, 443); !ok || addr != "127.0.0.1:8443" {
		t.Errorf("443 at %q %v", addr, ok)
	}
	if len(k3s.Ports) != 2 {
		t.Errorf("the sandbox given was changed: %v", k3s.Ports)
	}
	// Twice on adds nothing; a port moved by hand is kept as it is.
	moved := on
	moved.Ports = []string{"127.0.0.1:6443:6443", "127.0.0.1:9080:80"}
	if got := WithServiceLB(moved, true).Ports; len(got) != 3 || got[1] != "127.0.0.1:9080:80" || got[2] != "127.0.0.1:8443:443" {
		t.Errorf("on again %v", got)
	}
	off := WithServiceLB(on, false)
	if ServiceLBOf(off) || off.Command != k3s.Command || len(off.Ports) != 2 {
		t.Errorf("off again: %q %v", off.Command, off.Ports)
	}
	if made := KubernetesNodeWith(true); !ServiceLBOf(made) || made.Command != on.Command || len(made.Ports) != 4 {
		t.Errorf("made with ServiceLB: %q %v", made.Command, made.Ports)
	}

	// Other forms of --disable, and none at all.
	for cmd, want := range map[string]bool{
		"server --disable traefik --disable servicelb": false,
		"server --disable=servicelb,traefik":           false,
		"server --disable=traefik":                     true,
		"server":                                       true,
	} {
		spec := sandbox.Spec{Command: cmd}
		if got := ServiceLBOf(spec); got != want {
			t.Errorf("%q: %v", cmd, got)
		}
		if got := ServiceLBOf(WithServiceLB(spec, !want)); got != !want {
			t.Errorf("%q turned %v: %q", cmd, !want, WithServiceLB(spec, !want).Command)
		}
	}
	if got := WithServiceLB(sandbox.Spec{Command: "server --disable traefik --disable servicelb"}, true).Command; got != "server --disable=traefik" {
		t.Errorf("on from two flags: %q", got)
	}
	if got := WithServiceLB(sandbox.Spec{Command: "server --token=x"}, false).Command; got != "server --disable=servicelb --token=x" {
		t.Errorf("off without --disable: %q", got)
	}
}

func TestNodePorts(t *testing.T) {
	k3s := KubernetesNode()
	for port, want := range map[int]string{30000: "127.0.0.1:30000", 30019: "127.0.0.1:30019", 30020: "", 30080: "", 29999: "", 6443: "127.0.0.1:6443"} {
		got, ok := HostAddress(k3s, port)
		if got != want || ok != (want != "") {
			t.Errorf("%d: %q %v, want %q", port, got, ok, want)
		}
	}
	if got := strings.Join(NodePortRanges(k3s), ","); got != "30000–30019" {
		t.Errorf("ranges %s", got)
	}

	// Widened, moved and published in other forms: what the spec says counts.
	k3s.Ports = []string{"127.0.0.1:6443:6443", "31000-31004:30080-30084", "0.0.0.0:32000:32000/tcp", "30500/udp",
		"[::1]:30600:30600", "30700", "unix:~/.gvx/x.sock:30800", "nonsense"}
	for port, want := range map[int]string{
		30080: "127.0.0.1:31000", 30084: "127.0.0.1:31004", 30085: "", 32000: "127.0.0.1:32000",
		30500: "", 30600: "[::1]:30600", 30700: "127.0.0.1:30700", 30800: "", 30000: "",
	} {
		got, ok := HostAddress(k3s, port)
		if got != want || ok != (want != "") {
			t.Errorf("%d: %q %v, want %q", port, got, ok, want)
		}
	}
	if got := strings.Join(NodePortRanges(k3s), ","); got != "30080–30084,32000,30600,30700" {
		t.Errorf("ranges %s", got)
	}
}
