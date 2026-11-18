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

// The engine's API is published as asked, read back from the sandbox, and
// moved without touching the ports beside it.
func TestDockerAPI(t *testing.T) {
	socket := DockerAPI{Port: DefaultDockerPort}
	tcp := DockerAPI{TCP: true, Port: 23750}
	if got := socket.Publish(); got != "unix:~/.gvx/docker.sock:2375" {
		t.Errorf("socket publishes %q", got)
	}
	if got := tcp.Publish(); got != "127.0.0.1:23750:2375" {
		t.Errorf("tcp publishes %q", got)
	}
	if got := tcp.Host(); got != "tcp://127.0.0.1:23750" {
		t.Errorf("tcp host %q", got)
	}
	if got := (DockerAPI{TCP: true}).Host(); got != "tcp://127.0.0.1:2375" {
		t.Errorf("a port left out: %q", got)
	}

	spec := DockerEngineWith(socket)
	if api, ok := DockerAPIOf(spec); !ok || api.TCP {
		t.Errorf("read back %+v, %v", api, ok)
	}
	moved := WithDockerAPI(spec, tcp)
	if api, ok := DockerAPIOf(moved); !ok || api != tcp {
		t.Errorf("after the move: %+v, %v", api, ok)
	}
	if len(moved.Ports) != len(spec.Ports) || moved.Ports[1] != spec.Ports[1] {
		t.Errorf("the other ports changed: %v", moved.Ports)
	}
	if spec.Ports[0] != socket.Publish() {
		t.Errorf("the sandbox given was changed: %v", spec.Ports)
	}
	// A sandbox whose API port someone removed gets it back.
	bare := spec
	bare.Ports = []string{"127.0.0.1:9000-9019:9000-9019"}
	if got := WithDockerAPI(bare, tcp).Ports; len(got) != 2 || got[0] != tcp.Publish() {
		t.Errorf("a sandbox without the port: %v", got)
	}
	if _, ok := DockerAPIOf(bare); ok {
		t.Error("an API was read from a sandbox that publishes none")
	}
}

func TestNodePorts(t *testing.T) {
	k3s := KubernetesNode()
	for port, want := range map[int]string{30000: "127.0.0.1:30000", 30019: "127.0.0.1:30019", 30020: "", 30080: "", 29999: "", 6443: "127.0.0.1:6443"} {
		got, ok := NodePortAddress(k3s, port)
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
		got, ok := NodePortAddress(k3s, port)
		if got != want || ok != (want != "") {
			t.Errorf("%d: %q %v, want %q", port, got, ok, want)
		}
	}
	if got := strings.Join(NodePortRanges(k3s), ","); got != "30080–30084,32000,30600,30700" {
		t.Errorf("ranges %s", got)
	}
}
