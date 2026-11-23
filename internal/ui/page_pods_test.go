package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestPodsAccess(t *testing.T) {
	k3s := recipes.KubernetesNode()
	docker := recipes.DockerEngine()
	for _, tc := range []struct {
		name string
		rows []Row
		want PodsAccess
	}{
		{"no sandboxes", nil, PodsNoCluster},
		{"only docker", []Row{{Spec: docker, State: sandbox.State{Status: sandbox.Running}}}, PodsNoCluster},
		{"never started", []Row{{Spec: k3s}}, PodsStopped},
		{"starting", []Row{{Spec: k3s, State: sandbox.State{Status: sandbox.Starting}}}, PodsStarting},
		{"running", []Row{{Spec: docker}, {Spec: k3s, State: sandbox.State{Status: sandbox.Running}}}, PodsOpen},
		{"stopping", []Row{{Spec: k3s, State: sandbox.State{Status: sandbox.Stopping}}}, PodsStopped},
		{"failed", []Row{{Spec: k3s, State: sandbox.State{Status: sandbox.Failed, ExitCode: 1}}}, PodsStopped},
	} {
		got := podsAccess(tc.rows)
		if got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if open := got == PodsOpen; open != (got.Reason() == "") {
			t.Errorf("%s: open %v with the reason %q", tc.name, open, got.Reason())
		}
	}
}

func testPod(ns, name, phase string) kubeapi.Pod {
	p := kubeapi.Pod{Metadata: kubeapi.Meta{Namespace: ns, Name: name}, Spec: kubeapi.PodSpec{Containers: []kubeapi.Container{{Name: "c"}}}}
	p.Status.Phase = phase
	if phase == "Running" {
		p.Status.Containers = []kubeapi.ContainerStatus{{Name: "c", Ready: true, State: kubeapi.ContainerState{Running: &kubeapi.Running{}}}}
	}
	return p
}

func TestSortAndFilterPods(t *testing.T) {
	list := sortPods([]kubeapi.Pod{
		testPod("kube-system", "coredns-ccb96694c-7xk2m", "Running"),
		testPod("shop", "redis-0", "Running"),
		testPod("default", "web-1", "Running"),
		testPod("default", "api-1", "Pending"),
		testPod("kube-system", "local-path-provisioner-5cf85fd84d-x7k2p", "Running"),
	})
	keys := func(l []kubeapi.Pod) string {
		var out []string
		for _, p := range l {
			out = append(out, p.Key())
		}
		return strings.Join(out, ",")
	}
	if got := keys(list); got != "default/api-1,default/web-1,shop/redis-0,kube-system/coredns-ccb96694c-7xk2m,kube-system/local-path-provisioner-5cf85fd84d-x7k2p" {
		t.Errorf("sorted %s", got)
	}
	if got := keys(userPods(list, false)); got != "default/api-1,default/web-1,shop/redis-0" {
		t.Errorf("without the system's %s", got)
	}
	if got := len(userPods(list, true)); got != 5 {
		t.Errorf("with the system's %d", got)
	}
	for q, want := range map[string]string{
		"":         keys(list),
		" SHOP ":   "shop/redis-0",
		"pending":  "default/api-1",
		"kube-sys": "kube-system/coredns-ccb96694c-7xk2m,kube-system/local-path-provisioner-5cf85fd84d-x7k2p",
		"web":      "default/web-1",
		"nothing":  "",
	} {
		if got := keys(filterPods(list, q)); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
	if running, total, ns := podCounts(list); running != 4 || total != 5 || ns != 3 {
		t.Errorf("counts %d %d %d", running, total, ns)
	}
	if p := chosenPod(list, "shop/redis-0"); p.Name() != "redis-0" {
		t.Errorf("chosen %s", p.Key())
	}
	if p := chosenPod(userPods(list, false), "kube-system/coredns-ccb96694c-7xk2m"); p.Key() != "default/api-1" {
		t.Errorf("a hidden pod chosen leaves the first shown: %s", p.Key())
	}
}

func TestPodDeleteTextAndCommands(t *testing.T) {
	p := testPod("default", "web-7d4b9c8f6d-2m9qz", "Running")
	if got := podDeleteText(p); got != "Nothing owns it, so nothing starts it again." {
		t.Errorf("no owner: %s", got)
	}
	p.Metadata.Labels = map[string]string{"pod-template-hash": "7d4b9c8f6d"}
	p.Metadata.Owners = []kubeapi.OwnerRef{{Kind: "ReplicaSet", Name: "web-7d4b9c8f6d", Controller: true}}
	if got := podDeleteText(p); got != "Its Deployment web starts a new one in its place." {
		t.Errorf("a Deployment's: %s", got)
	}
	p.Metadata.Owners = []kubeapi.OwnerRef{{Kind: "Job", Name: "migrate", Controller: true}}
	if got := podDeleteText(p); got != "Its Job migrate starts another only if it has not finished." {
		t.Errorf("a Job's: %s", got)
	}

	home := rungv.HomeDir
	rungv.HomeDir = func() (string, error) { return "/Users/me", nil }
	defer func() { rungv.HomeDir = home }()
	for _, tc := range []struct {
		link LinkView
		want string
	}{
		{LinkView{C: CLIContext{Exists: true, InUse: true}}, "kubectl logs -f -n default web-7d4b9c8f6d-2m9qz -c c"},
		{LinkView{C: CLIContext{Exists: true}}, "kubectl --context gvx-k3s logs -f -n default web-7d4b9c8f6d-2m9qz -c c"},
		{LinkView{}, "kubectl --kubeconfig /Users/me/.gvx/k3s/kubeconfig.yaml logs -f -n default web-7d4b9c8f6d-2m9qz -c c"},
	} {
		if got := kubectlLogs(tc.link, p, "c"); got != tc.want {
			t.Errorf("logs: %s, want %s", got, tc.want)
		}
	}
	in := LinkView{C: CLIContext{Exists: true, InUse: true}}
	if got := kubectlExec(in, p, "c"); got != "kubectl exec -it -n default web-7d4b9c8f6d-2m9qz -c c -- sh" {
		t.Errorf("exec: %s", got)
	}
	if got := kubectlDescribe(in, p); got != "kubectl describe pod -n default web-7d4b9c8f6d-2m9qz" {
		t.Errorf("describe: %s", got)
	}
}

func TestPodLogContainer(t *testing.T) {
	p := testPod("default", "web", "Running")
	p.Spec.Containers = append(p.Spec.Containers, kubeapi.Container{Name: "exporter"})
	p.Spec.InitContainers = []kubeapi.Container{{Name: "init-db"}}
	for chosen, want := range map[string]string{"": "c", "exporter": "exporter", "init-db": "init-db", "gone": "c"} {
		if got := podLogContainer(p, chosen); got != want {
			t.Errorf("%q: %s, want %s", chosen, got, want)
		}
	}
	if id := podLogsID(p, "c"); id != "default/web/c" {
		t.Errorf("id %s", id)
	}
	if id := podLogsID(kubeapi.Pod{}, "c"); id != "" {
		t.Errorf("no pod, id %s", id)
	}
	at := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	lines := podLogLines([]kubeapi.LogLine{{Time: at, Text: "a"}, {Time: at, Text: "b"}, {Text: "c"}})
	if len(lines) != 3 || lines[1].Seq <= lines[0].Seq || lines[2].Seq <= lines[1].Seq || lines[0].Stream != sandbox.Stdout {
		t.Errorf("lines %+v", lines)
	}
}

func TestMenuListsPods(t *testing.T) {
	k3s := Row{Spec: recipes.KubernetesNode(), State: sandbox.State{Status: sandbox.Running}}
	var pods []kubeapi.Pod
	for i := range 12 {
		pods = append(pods, testPod("default", "web-"+itoa(i+10), "Running"))
	}
	pods = append(pods, testPod("kube-system", "coredns", "Running"))
	l := Listing[kubeapi.Pod]{Loaded: true, Items: pods}
	s := menuState([]Row{k3s}, Listing[dockerapi.Container]{}, l, false, LinkView{}, LinkView{})
	if !s.PodsListed || len(s.Pods) != maxMenuContainers || s.PodsMore != 3 || s.Pods[0].Pod.Name() != "web-10" {
		t.Errorf("menu pods %d more %d", len(s.Pods), s.PodsMore)
	}
	if s := menuState([]Row{k3s}, Listing[dockerapi.Container]{}, Listing[kubeapi.Pod]{}, true, LinkView{}, LinkView{}); !s.PodsWaiting || s.PodsListed {
		t.Errorf("waiting %+v", s)
	}
	stopped := Row{Spec: k3s.Spec}
	if s := menuState([]Row{stopped}, Listing[dockerapi.Container]{}, l, false, LinkView{}, LinkView{}); s.PodsListed || len(s.Pods) != 0 {
		t.Errorf("a stopped cluster lists pods: %+v", s.Pods)
	}
}

// The Pods item of the sidebar, the palette's entry and the page itself
// follow the cluster: open while its sandbox runs, greyed out with the
// reason in a tip otherwise. The page left open as the cluster stops
// stays, and says so; system pods are hidden until asked for.
func TestThePodsItemFollowsTheCluster(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	k3s := recipes.KubernetesNode()
	if err := st.SaveSandboxes([]sandbox.Spec{k3s}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	status := func(s sandbox.Status) {
		m.States.Set(map[string]sandbox.State{k3s.ID: {Status: s}})
		p.Frame()
	}
	listed := func() bool {
		_, ok := p.Semantics().Find(ggui.RoleTab, "Pods")
		return ok
	}
	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if n.Name == text {
				return true
			}
		}
		return false
	}

	for _, s := range []sandbox.Status{sandbox.Stopped, sandbox.Starting, sandbox.Stopping, sandbox.Exited, sandbox.Failed} {
		status(s)
		if listed() {
			t.Errorf("%v: the sidebar lists Pods", s)
		}
		m.Go(PagePods)
		if ggui.Peek(m.Page) == PagePods {
			t.Errorf("%v: Go opened the page", s)
			m.Page.Set(PageDashboard)
		}
	}

	status(sandbox.Running)
	if !listed() {
		t.Fatalf("running: the sidebar does not list Pods:\n%s", p.Semantics())
	}
	// k3s has yet to answer.
	m.PodsWaiting.Set(true)
	m.Go(PagePods)
	if ggui.Peek(m.Page) != PagePods {
		t.Fatal("running: Go did not open the page")
	}
	p.Frame()
	if !shows("The cluster is starting…") {
		t.Errorf("the page does not say the cluster is starting:\n%s", p.Semantics())
	}

	m.PodsWaiting.Set(false)
	m.Pods.Set(Listing[kubeapi.Pod]{Loaded: true, Items: sortPods([]kubeapi.Pod{
		testPod("default", "web-1", "Running"),
		testPod("kube-system", "coredns-ccb96694c-7xk2m", "Running"),
	})})
	p.Frame()
	if n := ggui.Peek(m.RunningPods); n != 2 {
		t.Errorf("the item's badge counts %d running pods", n)
	}
	if !shows("web-1") || shows("coredns-ccb96694c-7xk2m") {
		t.Errorf("the page shows the system's pods, or not the user's:\n%s", p.Semantics())
	}
	m.SystemPods.Set(true)
	p.Frame()
	if !shows("coredns-ccb96694c-7xk2m") {
		t.Errorf("System pods on, the page does not show coredns:\n%s", p.Semantics())
	}

	// Only system pods, hidden: the normal Pods table and System pods switch
	// stay visible (no separate "Show system pods" empty-state button).
	m.SystemPods.Set(false)
	m.Pods.Set(Listing[kubeapi.Pod]{Loaded: true, Items: []kubeapi.Pod{testPod("kube-system", "coredns-ccb96694c-7xk2m", "Running")}})
	p.Frame()
	if shows("Show system pods") || shows("No pods of your own") {
		t.Errorf("expected normal table with switch only, not Show system pods button:\n%s", p.Semantics())
	}
	if !shows("System pods") || shows("coredns-ccb96694c-7xk2m") {
		t.Errorf("expected System pods switch visible and coredns filtered out:\n%s", p.Semantics())
	}
	m.SystemPods.Set(true)
	p.Frame()
	if !shows("coredns-ccb96694c-7xk2m") {
		t.Errorf("toggling System pods switch on should show coredns:\n%s", p.Semantics())
	}
	m.SystemPods.Set(false)

	// Workloads and Services also always render their normal table + System
	// switch instead of a "Show system ..." empty-state button.
	m.Workloads.Set(Listing[kubeapi.Workload]{Loaded: true, Items: []kubeapi.Workload{
		{Kind: kubeapi.KindDeployment, Metadata: kubeapi.Meta{Namespace: "kube-system", Name: "coredns"}, Desired: 1, Ready: 1, UpToDate: 1},
	}})
	m.GoKube(KubeWorkloads)
	p.Frame()
	if shows("Show system workloads") || shows("No workloads of your own") {
		t.Errorf("Workloads should show normal table with switch only:\n%s", p.Semantics())
	}
	if !shows("System workloads") || shows("coredns") {
		t.Errorf("Workloads should show System workloads switch and hide coredns:\n%s", p.Semantics())
	}
	m.SystemPods.Set(true)
	p.Frame()
	if !shows("coredns") {
		t.Errorf("toggling System workloads on should show coredns:\n%s", p.Semantics())
	}
	m.SystemPods.Set(false)

	m.Services.Set(Listing[kubeapi.Service]{Loaded: true, Items: []kubeapi.Service{
		{Metadata: kubeapi.Meta{Namespace: "kube-system", Name: "kube-dns"}, Spec: kubeapi.ServiceSpec{Type: "ClusterIP"}},
	}})
	m.GoKube(KubeServices)
	p.Frame()
	if shows("Show system services") || shows("No services of your own") {
		t.Errorf("Services should show normal table with switch only:\n%s", p.Semantics())
	}
	if !shows("System services") || shows("kube-dns") {
		t.Errorf("Services should show System services switch and hide kube-dns:\n%s", p.Semantics())
	}
	m.Go(PagePods)
	p.Frame()

	// The palette's entry is open too, and is greyed out with the item.
	m.PaletteOpen.Set(true)
	entry := func() ggui.SemNode {
		for _, n := range p.Semantics().Nodes(ggui.RoleMenuItem) {
			if n.Name == "Pods" {
				return n
			}
		}
		t.Fatalf("no Pods entry in the palette:\n%s", p.Semantics())
		return ggui.SemNode{}
	}
	if entry().Disabled {
		t.Error("running: the palette's entry is disabled")
	}
	status(sandbox.Stopping)
	if !entry().Disabled {
		t.Error("stopping: the palette's entry is enabled")
	}
	m.PaletteOpen.Set(false)

	// The cluster stopped under the page: it stays, and says what to do.
	m.Pods.Set(Listing[kubeapi.Pod]{})
	status(sandbox.Stopped)
	if ggui.Peek(m.Page) != PagePods {
		t.Errorf("the page closed: %s", ggui.Peek(m.Page))
	}
	if !shows("Kubernetes is not running") || !shows("Start Kubernetes") || !shows("Open Kubernetes page") {
		t.Errorf("the page does not tell the cluster stopped:\n%s", p.Semantics())
	}
}

// Until the cluster's API server has answered, in the first minutes after
// its sandbox started, the page waits for it rather than tell of an
// error: first for the kubeconfig k3s writes, then for the server.
func TestLoadPodsWaitsForTheCluster(t *testing.T) {
	dir := t.TempDir()
	home := rungv.HomeDir
	rungv.HomeDir = func() (string, error) { return dir, nil }
	defer func() { rungv.HomeDir = home }()
	st := store.Store{Dir: dir}
	k3s := recipes.KubernetesNode()
	if err := st.SaveSandboxes([]sandbox.Spec{k3s}); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return widget.Box() }, ggui.Sz(100, 100))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()
	load := func(started time.Time) {
		t.Helper()
		m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Running, StartedAt: started}})
		m.Pods.Set(Listing[kubeapi.Pod]{})
		m.PodsWaiting.Set(false)
		m.LoadPods()
		for i := 0; m.podsLoading; i++ {
			if i > 500 {
				t.Fatal("LoadPods did not finish")
			}
			time.Sleep(10 * time.Millisecond)
			p.Frame()
		}
	}

	load(time.Now())
	if !ggui.Peek(m.PodsWaiting) || ggui.Peek(m.KubeconfigReady) {
		t.Errorf("no kubeconfig: waiting %v, ready %v", ggui.Peek(m.PodsWaiting), ggui.Peek(m.KubeconfigReady))
	}

	// A kubeconfig, and nothing answers at its server.
	cfg := "apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:1\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\ncurrent-context: default\nkind: Config\npreferences: {}\nusers:\n- name: default\n  user:\n    token: x\n"
	if err := os.MkdirAll(recipes.KubeconfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipes.Kubeconfig(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	load(time.Now().Add(-30 * time.Second))
	if !ggui.Peek(m.PodsWaiting) || !ggui.Peek(m.KubeconfigReady) || ggui.Peek(m.Pods).Err != "" {
		t.Errorf("just started: waiting %v, error %q", ggui.Peek(m.PodsWaiting), ggui.Peek(m.Pods).Err)
	}
	load(time.Now().Add(-10 * time.Minute))
	if ggui.Peek(m.PodsWaiting) || ggui.Peek(m.Pods).Err == "" {
		t.Errorf("long started: waiting %v, error %q", ggui.Peek(m.PodsWaiting), ggui.Peek(m.Pods).Err)
	}

	// The cluster stopped: nothing is listed, nor waited for.
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Stopped}})
	m.PodsWaiting.Set(true)
	m.LoadPods()
	if ggui.Peek(m.PodsWaiting) || ggui.Peek(m.Pods).Loaded || ggui.Peek(m.Pods).Err != "" {
		t.Errorf("stopped: %+v", ggui.Peek(m.Pods))
	}
}
