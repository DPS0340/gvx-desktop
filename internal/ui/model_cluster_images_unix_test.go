//go:build !windows

package ui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// importControl is a node's control that keeps what each exec was given
// on its standard input.
type importControl struct {
	sandbox.Control
	id  string
	mu  *sync.Mutex
	got map[string]string // node ID → argv and input
}

func (c importControl) Exec(_ context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
	return &importProc{done: func(in string) {
		c.mu.Lock()
		c.got[c.id] = strings.Join(o.Argv, " ") + "|" + in
		c.mu.Unlock()
	}}, nil
}

type importProc struct {
	sandbox.ExecProcess
	in   bytes.Buffer
	done func(string)
}

func (p *importProc) Stdin() io.WriteCloser       { return p }
func (p *importProc) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *importProc) Close() error                { return nil }
func (p *importProc) Stdout() io.Reader           { return strings.NewReader("") }
func (p *importProc) Stderr() io.Reader           { return strings.NewReader("") }
func (p *importProc) Wait() (int, error)          { p.done(p.in.String()); return 0, nil }

// An image of the Docker Engine goes, as docker save writes it, into every
// running node of the cluster.
func TestLoadIntoCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rungvPath, _ := fakeNodeRungv(t)
	m, p := launch(t, store.Store{Dir: t.TempDir()}, rungvPath)
	m.sup.StopGrace = nil
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/images/acme/app:dev/get" {
			_, _ = w.Write([]byte("docker save of acme/app:dev"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(engine.Close)
	m.dockerBackend = "tcp://" + strings.TrimPrefix(engine.URL, "http://")
	var mu sync.Mutex
	got := map[string]string{}
	m.controlOf = func(id string) (sandbox.Control, bool) { return importControl{id: id, mu: &mu, got: got}, true }

	img := DockerImageRow{ID: "sha256:1", Repository: "acme/app", Tag: "dev"}
	if why := LoadBlocked(img, ggui.Peek(m.Rows)); why == "" {
		t.Error("loadable with no cluster")
	}
	if why := LoadBlocked(DockerImageRow{ID: "sha256:2", Repository: "<none>", Tag: "<none>", Dangling: true}, nil); why == "" {
		t.Error("an untagged image is loadable")
	}

	server := recipes.WithNodeNetwork(recipes.KubernetesNode())
	agent := recipes.KubernetesAgentNode(server, 1, "t")
	m.saveSpecs([]sandbox.Spec{server, agent})
	m.StartSandbox(server.ID)
	until(t, p, "the cluster to be up, as the model shows it", func() bool {
		nodes, _ := loadTargets(ggui.Peek(m.Rows))
		return len(nodes) == 2
	})
	if why := LoadBlocked(img, ggui.Peek(m.Rows)); why != "" {
		t.Fatalf("not loadable: %s", why)
	}
	m.LoadIntoCluster(img)
	until(t, p, "both nodes to import it", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
	for _, id := range []string{server.ID, agent.ID} {
		if want := "ctr -n k8s.io images import -|docker save of acme/app:dev"; got[id] != want {
			t.Errorf("node %s got %q", id, got[id])
		}
	}
}
