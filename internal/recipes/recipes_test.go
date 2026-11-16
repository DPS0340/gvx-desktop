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
