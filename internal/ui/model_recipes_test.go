package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// recipeModel is an offline model on a store of its own, drawn by a probe.
func recipeModel(t *testing.T) (*Model, *ggui.Probe, store.Store) {
	t.Helper()
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p, st
}

// The load balancer is the settings' until there is a cluster, and its
// sandbox's from then on; turning it on rewrites the sandbox, keeps what
// the user changed of it, and is saved.
func TestSetServiceLBMovesTheSettingAndTheSandbox(t *testing.T) {
	m, p, st := recipeModel(t)
	if m.ServiceLB() {
		t.Fatal("on with no cluster and no setting")
	}
	m.SetServiceLB(true)
	p.Frame()
	if !m.ServiceLB() {
		t.Fatal("the setting alone did not turn it on")
	}
	m.SetServiceLB(false)

	spec := recipes.KubernetesNodeWith(ggui.Peek(m.Settings).K3sServiceLB)
	spec.Memory = "4g"
	m.saveSpecs([]sandbox.Spec{spec})
	p.Frame()
	m.SetServiceLB(true)
	p.Frame()
	saved, err := st.Sandboxes()
	if err != nil || len(saved) != 1 {
		t.Fatalf("saved sandboxes: %v, %v", saved, err)
	}
	if !recipes.ServiceLBOf(saved[0]) || saved[0].Memory != "4g" || len(saved[0].Ports) != 4 {
		t.Errorf("saved %q %v %q", saved[0].Command, saved[0].Ports, saved[0].Memory)
	}
	if set, _ := st.Settings(); !set.K3sServiceLB {
		t.Error("the setting was not saved")
	}
	// The sandbox's command counts over the setting.
	m.UpdateSettings(func(s *store.Settings) { s.K3sServiceLB = false })
	if !m.ServiceLB() {
		t.Error("the setting won over the sandbox")
	}
}

// A LoadBalancer service is reached on its own port while ServiceLB runs
// and the sandbox publishes the port, and on its NodePort otherwise.
func TestLoadBalancerReach(t *testing.T) {
	SetLanguage(LangEnglish)
	svc := kubeapi.Service{Metadata: kubeapi.Meta{Namespace: "default", Name: "web"}, Spec: kubeapi.ServiceSpec{Type: "LoadBalancer", Ports: []kubeapi.ServicePort{
		{Port: 80, NodePort: 30005, Protocol: "TCP"},
		{Port: 443, NodePort: 31443},
		{Port: 5432, NodePort: 31432},
	}}}
	off := recipes.KubernetesNode()
	got := nodePortReach(svc, off)
	if len(got) != 3 || got[0] != (NodePortReach{Port: 30005, Address: "127.0.0.1:30005"}) || got[1].LB || got[1].Address != "" {
		t.Errorf("ServiceLB off: %+v", got)
	}
	on := recipes.WithServiceLB(off, true)
	got = nodePortReach(svc, on)
	if len(got) != 3 || got[0] != (NodePortReach{Port: 80, LB: true, Address: "127.0.0.1:8080"}) ||
		got[1] != (NodePortReach{Port: 443, LB: true, Address: "127.0.0.1:8443"}) {
		t.Fatalf("ServiceLB on: %+v", got)
	}
	if got[0].URL() != "http://127.0.0.1:8080" || got[1].URL() != "https://127.0.0.1:8443" {
		t.Errorf("urls %s %s", got[0].URL(), got[1].URL())
	}
	if !got[2].LB || got[2].Port != 5432 || got[2].Address != "" ||
		got[2].Why != "ServiceLB takes port 5432 on the node, and the Kubernetes sandbox does not publish it. Add it to the sandbox's ports in its settings, or forward a port." {
		t.Errorf("an unpublished port: %+v", got[2])
	}
	// A NodePort the sandbox publishes still reaches it when the port does not.
	svc.Spec.Ports[2].NodePort = 30010
	if got := nodePortReach(svc, on); got[2] != (NodePortReach{Port: 30010, Address: "127.0.0.1:30010"}) {
		t.Errorf("through the NodePort: %+v", got[2])
	}
	// A NodePort service is the same with ServiceLB on.
	svc.Spec.Type = "NodePort"
	if got := nodePortReach(svc, on); got[0] != (NodePortReach{Port: 30005, Address: "127.0.0.1:30005"}) {
		t.Errorf("a NodePort service: %+v", got[0])
	}
}

// The shared folders are the settings' until there is an engine, the home
// folder unless they say otherwise, and its sandbox's from then on; a
// change rewrites the sandbox's shared mounts and nothing else of it.
func TestSharedFoldersMoveTheSettingAndTheSandbox(t *testing.T) {
	m, p, st := recipeModel(t)
	home := rungv.ExpandHome("~")
	if got := m.SharedFolders(); !slices.Equal(got, []string{home}) {
		t.Fatalf("with no engine and no setting: %v", got)
	}
	m.SetSharedFolders(nil)
	if got := m.SharedFolders(); len(got) != 0 {
		t.Fatalf("none is not the default: %v", got)
	}
	if set, _ := st.Settings(); set.DockerSharedFolders == nil {
		t.Error("none was saved as the default")
	}

	dir := t.TempDir()
	m.ShareFolder(dir)
	spec := recipes.WithSharedFolders(recipes.DockerEngine(), sharedSetting(ggui.Peek(m.Settings)))
	if got := recipes.SharedFoldersOf(spec); !slices.Equal(got, []string{dir}) {
		t.Fatalf("a new engine shares %v", got)
	}
	m.saveSpecs([]sandbox.Spec{spec})
	p.Frame()

	inside := filepath.Join(dir, "src")
	os.Mkdir(inside, 0o755)
	m.ShareFolder(inside) // shared already, through dir
	if got := m.SharedFolders(); !slices.Equal(got, []string{dir}) {
		t.Errorf("a folder inside one was added: %v", got)
	}
	m.ShareFolder(home)
	saved, _ := st.Sandboxes()
	if got := recipes.SharedFoldersOf(saved[0]); !slices.Equal(got, []string{dir, home}) {
		t.Errorf("saved %v", got)
	}
	if saved[0].Mounts[0].Target != "/var/lib/docker" || len(saved[0].Mounts) != 3 {
		t.Errorf("the other mounts: %+v", saved[0].Mounts)
	}
	m.UnshareFolder(dir)
	if got := m.SharedFolders(); !slices.Equal(got, []string{home}) {
		t.Errorf("after unsharing %v", got)
	}
	// The sandbox's mounts count over the setting.
	m.UpdateSettings(func(s *store.Settings) { s.DockerSharedFolders = nil })
	if got := m.SharedFolders(); !slices.Equal(got, []string{home}) {
		t.Errorf("the setting won over the sandbox: %v", got)
	}
}

func TestShareProblem(t *testing.T) {
	SetLanguage(LangEnglish)
	for _, c := range []struct {
		dir, want string
	}{
		{"/usr/local", "The sandbox's own system is at that path, or inside it. Share a folder of your own."},
		{"relative", "Choose a folder by its full path, on a drive of this computer."},
		{filepath.Join(t.TempDir(), "gone"), "No folder is at that path."},
	} {
		_, err := recipes.AddSharedFolder(nil, c.dir)
		if got := shareProblem(err); got != c.want {
			t.Errorf("%s: %q", c.dir, got)
		}
	}
	dir := t.TempDir()
	_, err := recipes.AddSharedFolder([]string{dir}, dir)
	if got := shareProblem(err); got != "It is shared already, through "+tildePath(dir)+"." {
		t.Errorf("shared: %q", got)
	}
}

// A container's bind mount outside the shared folders is flagged, unless
// it is of the sandbox's own system.
func TestUnsharedBinds(t *testing.T) {
	engine := recipes.WithSharedFolders(recipes.DockerEngine(), []string{"/Users/me"})
	c := dockerapi.Container{Mounts: []dockerapi.Mount{
		{Type: "bind", Source: "/Users/me/shop/site", Destination: "/usr/share/nginx/html"},
		{Type: "bind", Source: "/Volumes/work/db", Destination: "/data"},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
		{Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Destination: "/x"},
	}}
	if got := unsharedBinds(c, engine); len(got) != 1 || got[0].Source != "/Volumes/work/db" {
		t.Errorf("unshared %+v", got)
	}
	if got := unsharedBinds(c, sandbox.Spec{}); got != nil {
		t.Errorf("with no engine %+v", got)
	}
}

func TestRegistryRef(t *testing.T) {
	for _, c := range []struct{ repo, tag, want string }{
		{"app", "dev", "host.docker.internal:5001/app:dev"},
		{"acme/app", "1.0", "host.docker.internal:5001/acme/app:1.0"},
		{"ghcr.io/acme/api", "dev", "host.docker.internal:5001/acme/api:dev"},
		{"localhost:5001/app", "dev", "host.docker.internal:5001/app:dev"},
		{"host.docker.internal:5001/app", "dev", "host.docker.internal:5001/app:dev"},
		{"localhost/app", "x", "host.docker.internal:5001/app:x"},
	} {
		if got := RegistryRef(DockerImageRow{Repository: c.repo, Tag: c.tag}); got != c.want {
			t.Errorf("%s:%s -> %s", c.repo, c.tag, got)
		}
	}
}

// Recipe sandboxes saved by an earlier version start with the registry's
// flags from then on, kept as the user had them otherwise; the store is
// written once.
func TestOldRecipesAreUpgradedAtLoad(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	engine := recipes.DockerEngine()
	engine.Command, engine.Memory = "--tls=false", "6g"
	k3s := recipes.KubernetesNode()
	k3s.Command = "server --disable=traefik,servicelb --write-kubeconfig=/output/kubeconfig.yaml --write-kubeconfig-mode=644"
	web := sandbox.Spec{ID: "w", Name: "web", Image: "nginx", Command: "nginx -g 'daemon off;'", CreatedAt: k3s.CreatedAt.Add(time.Second)}
	if err := st.SaveSandboxes([]sandbox.Spec{engine, k3s, web}); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()
	saved, _ := st.Sandboxes()
	if len(saved) != 3 || saved[0].Command != "--tls=false --insecure-registry=host.docker.internal:5001" || saved[0].Memory != "6g" ||
		!strings.HasSuffix(saved[1].Command, " --private-registry=/output/registries.yaml") || recipes.ServiceLBOf(saved[1]) ||
		!saved[2].Equal(web) {
		t.Errorf("saved %+v", saved)
	}
	if got := ggui.Peek(m.Specs); len(got) != 3 || !got[0].Equal(saved[0]) {
		t.Errorf("the model has %+v", got)
	}
}

// Without the registry, pushing offers to set it up, and doing so makes
// its sandbox.
func TestPushOffersTheRegistry(t *testing.T) {
	m, p, st := recipeModel(t)
	SetLanguage(LangEnglish) // the model speaks the system's
	img := DockerImageRow{ID: "sha256:1", Repository: "app", Tag: "dev"}
	m.PushToRegistry(img)
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Set up the image registry?" {
		t.Fatalf("asked %q", ggui.Peek(m.ConfirmTitle))
	}
	m.RunConfirmed()
	p.Frame()
	saved, _ := st.Sandboxes()
	if len(saved) != 1 || saved[0].Recipe != recipes.Registry || saved[0].Image != "registry:2" {
		t.Fatalf("saved %+v", saved)
	}
	// Made, and stopped: pushing offers to start it.
	m.ConfirmOpen.Set(false)
	m.PushToRegistry(img)
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Start the image registry?" {
		t.Errorf("asked %q", ggui.Peek(m.ConfirmTitle))
	}
	// A dangling image has nothing to push.
	m.ConfirmOpen.Set(false)
	m.PushToRegistry(DockerImageRow{ID: "sha256:2", Repository: "<none>", Tag: "<none>", Dangling: true})
	if ggui.Peek(m.ConfirmOpen) {
		t.Error("a dangling image was offered a push")
	}
}
