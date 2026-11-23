package ui

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// ownedPod is a running pod of a workload: of a Deployment through its
// ReplicaSet, or of anything else directly.
func ownedPod(ns, name, kind, owner string) kubeapi.Pod {
	p := testPod(ns, name, "Running")
	p.Metadata.Labels = map[string]string{}
	if kind == kubeapi.KindDeployment {
		p.Metadata.Labels["pod-template-hash"] = "7d4b9c8f6d"
		kind, owner = "ReplicaSet", owner+"-7d4b9c8f6d"
	}
	p.Metadata.Owners = []kubeapi.OwnerRef{{Kind: kind, Name: owner, Controller: true}}
	return p
}

func TestWorkloadsAndTheirPods(t *testing.T) {
	SetLanguage(LangEnglish)
	pods := []kubeapi.Pod{
		ownedPod("default", "web-7d4b9c8f6d-a", kubeapi.KindDeployment, "web"),
		ownedPod("default", "web-7d4b9c8f6d-b", kubeapi.KindDeployment, "web"),
		ownedPod("default", "web-api-7d4b9c8f6d-c", kubeapi.KindDeployment, "web-api"),
		ownedPod("other", "web-7d4b9c8f6d-d", kubeapi.KindDeployment, "web"),
		ownedPod("shop", "redis-0", kubeapi.KindStatefulSet, "redis"),
		testPod("default", "alone", "Running"),
	}
	names := func(l []kubeapi.Pod) string {
		var out []string
		for _, p := range l {
			out = append(out, p.Name())
		}
		return strings.Join(out, ",")
	}
	for ref, want := range map[WorkloadRef]string{
		{}: names(pods),
		{kubeapi.KindDeployment, "default", "web"}:   "web-7d4b9c8f6d-a,web-7d4b9c8f6d-b",
		{kubeapi.KindStatefulSet, "shop", "redis"}:   "redis-0",
		{kubeapi.KindDeployment, "shop", "redis"}:    "",
		{kubeapi.KindJob, "default", "migrate-db"}:   "",
		{kubeapi.KindDeployment, "default", "alone"}: "",
	} {
		if got := names(ownedPods(pods, ref)); got != want {
			t.Errorf("%+v: %s, want %s", ref, got, want)
		}
	}

	w := func(kind, ns, name string, desired, ready int) kubeapi.Workload {
		return kubeapi.Workload{Kind: kind, Metadata: kubeapi.Meta{Namespace: ns, Name: name}, Desired: desired, Ready: ready, UpToDate: desired, Images: []string{"img:" + name}}
	}
	list := sortWorkloads([]kubeapi.Workload{
		w(kubeapi.KindDeployment, "kube-system", "coredns", 1, 1),
		w(kubeapi.KindStatefulSet, "shop", "redis", 1, 1),
		w(kubeapi.KindJob, "default", "web", 1, 1),
		w(kubeapi.KindDeployment, "default", "web", 2, 2),
		w(kubeapi.KindDeployment, "default", "api", 1, 0),
	})
	keys := func(l []kubeapi.Workload) string {
		var out []string
		for _, w := range l {
			out = append(out, w.Key())
		}
		return strings.Join(out, ",")
	}
	if got := keys(list); got != "Deployment/default/api,Deployment/default/web,Job/default/web,StatefulSet/shop/redis,Deployment/kube-system/coredns" {
		t.Errorf("sorted %s", got)
	}
	if got := len(userWorkloads(list, false)); got != 4 {
		t.Errorf("without the system's: %d", got)
	}
	for q, want := range map[string]string{"statefulset": "StatefulSet/shop/redis", "IMG:API": "Deployment/default/api", "kube-": "Deployment/kube-system/coredns"} {
		if got := keys(filterWorkloads(list, q)); got != want {
			t.Errorf("%q: %s", q, got)
		}
	}

	// A Deployment whose counts say it is on its way is failing when a pod
	// of it crashes; another's crashing pod does not count.
	api := w(kubeapi.KindDeployment, "default", "api", 1, 0)
	crash := ownedPod("default", "api-7d4b9c8f6d-x", kubeapi.KindDeployment, "api")
	crash.Status.Containers[0].Ready = false
	crash.Status.Containers[0].State = kubeapi.ContainerState{Waiting: &kubeapi.Waiting{Reason: "CrashLoopBackOff"}}
	if h, why := workloadHealth(api, []kubeapi.Pod{crash}); h != kubeapi.Failing || why != "api-7d4b9c8f6d-x: CrashLoopBackOff" {
		t.Errorf("api: %v %q", h, why)
	}
	if h, why := workloadHealth(api, nil); h != kubeapi.Changing || why != "0 of 1 ready" {
		t.Errorf("api with no pods: %v %q", h, why)
	}
	web := w(kubeapi.KindDeployment, "default", "web", 2, 2)
	if h, why := workloadHealth(web, []kubeapi.Pod{crash}); h != kubeapi.Healthy || why != "All 2 ready" {
		t.Errorf("web: %v %q", h, why)
	}
	if h, why := workloadHealth(w(kubeapi.KindDeployment, "default", "idle", 0, 0), nil); h != kubeapi.Unknown || why != "Scaled to 0" {
		t.Errorf("idle: %v %q", h, why)
	}
	if got := workloadDeleteText(web, 2); got != "Its 2 pods are deleted with it, and nothing starts them again." {
		t.Errorf("delete text %q", got)
	}
	if got := kubectlWorkload(LinkView{C: CLIContext{Exists: true, InUse: true}}, web, "scale", "--replicas=2"); got != "kubectl scale deployment/web -n default --replicas=2" {
		t.Errorf("kubectl %s", got)
	}
}

func TestNodePortReach(t *testing.T) {
	SetLanguage(LangEnglish)
	spec := recipes.KubernetesNode()
	svc := kubeapi.Service{Metadata: kubeapi.Meta{Namespace: "default", Name: "web"}, Spec: kubeapi.ServiceSpec{Type: "NodePort", Selector: map[string]string{"app": "web"}, Ports: []kubeapi.ServicePort{
		{Port: 80, NodePort: 30000, Protocol: "TCP"},
		{Port: 81, NodePort: 30080},
		{Port: 53, NodePort: 30001, Protocol: "UDP"},
		{Port: 82},
	}}}
	got := nodePortReach(svc, spec)
	if len(got) != 2 || got[0] != (NodePortReach{Port: 30000, Address: "127.0.0.1:30000"}) || got[1].Address != "" ||
		got[1].Why != "The Kubernetes sandbox publishes only NodePorts 30000–30019 to this computer, and 30080 is not among them. Widen the range in the sandbox settings, or forward a port." {
		t.Errorf("reach %+v", got)
	}
	// The range is the spec's: widened, 30080 reaches this computer.
	spec.Ports = []string{"127.0.0.1:6443:6443", "127.0.0.1:30000-30099:30000-30099"}
	if got := nodePortReach(svc, spec); got[1].Address != "127.0.0.1:30080" {
		t.Errorf("widened %+v", got)
	}
	spec.Ports = []string{"127.0.0.1:6443:6443"}
	if got := nodePortReach(svc, spec); !strings.HasPrefix(got[0].Why, "The Kubernetes sandbox publishes no NodePorts") {
		t.Errorf("none published %+v", got)
	}
	if forwardable(svc) != "" || forwardable(kubeapi.Service{}) == "" || forwardable(kubeapi.Service{Spec: kubeapi.ServiceSpec{Type: "ExternalName", Selector: map[string]string{"a": "b"}}}) == "" {
		t.Error("forwardable")
	}
	if got := portText(kubeapi.ServicePort{Port: 80, TargetPort: kubeapi.IntOrString{Str: "http"}}); got != "80→http/TCP" {
		t.Errorf("port %s", got)
	}
	if got := portText(kubeapi.ServicePort{Port: 53, Protocol: "UDP"}); got != "53→53/UDP" {
		t.Errorf("port %s", got)
	}
}

// kubeModel is a model with the cluster's sandbox running, whose port
// forwards a stand-in carries: each connection is answered with the name
// of what it is forwarded to, and closed.
func kubeModel(t *testing.T, offline bool) (*Model, *ggui.Probe, sandbox.Spec) {
	t.Helper()
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
	p := ggui.ProbeBuilder(func() ggui.Widget {
		if offline {
			return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
		}
		return widget.Box()
	}, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = offline
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Running}})
		m.forwarder = func(ctx context.Context, tg kubeapi.Target, ln net.Listener, report func(error)) error {
			stop := context.AfterFunc(ctx, func() { ln.Close() })
			defer stop()
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				fmt.Fprintf(c, "%s%s:%d\n", tg.Pod, tg.Service, tg.Port)
				c.Close()
				report(nil)
			}
		}
		m.followForwards()
	})
	t.Cleanup(func() { m.CloseForwards(); p.Close() })
	p.Frame()
	return m, p, k3s
}

// freePort is a port of 127.0.0.1 nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// dial reads what a connection to port says, or the error.
func dial(port int) (string, error) {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	return bufio.NewReader(c).ReadString('\n')
}

// settle runs frames until cond holds, for what the forwards post back.
func settle(t *testing.T, p *ggui.Probe, cond func() bool, what string) {
	t.Helper()
	for range 300 {
		p.Frame()
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited for %s", what)
}

func TestPortForwardLifecycle(t *testing.T) {
	m, p, k3s := kubeModel(t, false)
	web := kubeapi.Target{Namespace: "default", Pod: "web-1", Port: 80}

	// Started, it listens at once, and a connection reaches the target.
	port := freePort(t)
	f, err := m.StartForward(web, "uid-1", port)
	if err != nil || f.ID == 0 || f.Local != port || f.Address() != "127.0.0.1:"+itoa(port) {
		t.Fatalf("start: %+v %v", f, err)
	}
	if got, err := dial(port); got != "web-1:80\n" || err != nil {
		t.Errorf("through the forward: %q %v", got, err)
	}
	settle(t, p, func() bool { return len(ggui.Peek(m.Forwards)) == 1 && ggui.Peek(m.Forwards)[0].Served == 1 }, "the connection to be counted")

	// A port the app forwards already, and one another program holds.
	if _, err := m.StartForward(web, "", port); err == nil || !strings.Contains(err.Error(), "is forwarded already, to pod/web-1:80") {
		t.Errorf("the same port twice: %v", err)
	}
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	taken := busy.Addr().(*net.TCPAddr).Port
	if _, err := m.StartForward(web, "", taken); err == nil || err.Error() != "Port "+itoa(taken)+" of this computer is in use by another program. Choose another." {
		t.Errorf("a busy port: %v", err)
	}
	if len(ggui.Peek(m.Forwards)) != 1 {
		t.Errorf("forwards %+v", ggui.Peek(m.Forwards))
	}
	// The dialog offers the next free port for one taken, and says why
	// it cannot start.
	if got := m.suggestLocal(taken); got == taken {
		t.Errorf("offered the busy port %d", got)
	}
	if got := m.suggestLocal(80); got < 8080 {
		t.Errorf("offered %d for 80", got)
	}
	m.AskForwardPod(ownedPod("default", "web-1", kubeapi.KindDeployment, "web"))
	m.ForwardRemote.Set("80")
	m.ForwardLocal.Set(itoa(taken))
	m.SubmitForward()
	if !ggui.Peek(m.ForwardOpen) || !strings.Contains(ggui.Peek(m.ForwardErr), "in use by another program") {
		t.Errorf("the dialog: open %v, %q", ggui.Peek(m.ForwardOpen), ggui.Peek(m.ForwardErr))
	}
	m.ForwardLocal.Set("70000")
	m.SubmitForward()
	if ggui.Peek(m.ForwardErr) != "The local port is a number from 1 to 65535." {
		t.Errorf("a port past 65535: %q", ggui.Peek(m.ForwardErr))
	}
	m.ForwardOpen.Set(false)

	// Stopped, the port is closed.
	m.StopForward(f.ID, "")
	settle(t, p, func() bool { _, err := dial(port); return err != nil && len(ggui.Peek(m.Forwards)) == 0 }, "the port to close")

	// Its pod gone, or made anew under the same name, a forward stops; one
	// whose service is listed stays.
	a, _ := m.StartForward(web, "uid-1", freePort(t))
	svc, _ := m.StartForward(kubeapi.Target{Namespace: "default", Service: "web", Port: 80}, "", freePort(t))
	pod := testPod("default", "web-1", "Running")
	pod.Metadata.UID = "uid-1"
	service := kubeapi.Service{Metadata: kubeapi.Meta{Namespace: "default", Name: "web"}}
	m.checkForwards([]kubeapi.Pod{pod}, []kubeapi.Service{service}, true)
	if len(ggui.Peek(m.Forwards)) != 2 {
		t.Fatalf("stopped with its pod there: %+v", ggui.Peek(m.Forwards))
	}
	pod.Metadata.UID = "uid-2"
	m.checkForwards([]kubeapi.Pod{pod}, nil, false) // services not listed: the service's stays
	if fw := ggui.Peek(m.Forwards); len(fw) != 1 || fw[0].ID != svc.ID {
		t.Errorf("a pod made anew: %+v", fw)
	}
	m.checkForwards(nil, nil, true)
	if len(ggui.Peek(m.Forwards)) != 0 {
		t.Errorf("its service gone: %+v", ggui.Peek(m.Forwards))
	}
	settle(t, p, func() bool {
		_, e1 := dial(a.Local)
		_, e2 := dial(svc.Local)
		return e1 != nil && e2 != nil
	}, "their ports to close")

	// The cluster stops: every forward does.
	b, _ := m.StartForward(web, "", freePort(t))
	if got, _ := dial(b.Local); got != "web-1:80\n" {
		t.Errorf("before the cluster stops: %q", got)
	}
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Stopping}})
	p.Frame()
	if len(ggui.Peek(m.Forwards)) != 0 {
		t.Errorf("the cluster stopped: %+v", ggui.Peek(m.Forwards))
	}
	settle(t, p, func() bool { _, err := dial(b.Local); return err != nil }, "the port to close as the cluster stops")

	// The app quits: CloseForwards stops the rest, and waits for them.
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Running}})
	p.Frame()
	c, _ := m.StartForward(web, "", freePort(t))
	m.CloseForwards()
	if _, err := dial(c.Local); err == nil {
		t.Error("a forward outlived the app")
	}
	m.forwards.mu.Lock()
	live := len(m.forwards.stops)
	m.forwards.mu.Unlock()
	if live != 0 {
		t.Errorf("%d forwards still run", live)
	}
}

// A forward that ends on its own, as when its listener fails, is dropped
// and told of.
func TestPortForwardThatEnds(t *testing.T) {
	m, p, _ := kubeModel(t, false)
	m.forwarder = func(ctx context.Context, _ kubeapi.Target, ln net.Listener, report func(error)) error {
		ln.Close()
		report(fmt.Errorf("refused"))
		return fmt.Errorf("the listener broke")
	}
	if _, err := m.StartForward(kubeapi.Target{Namespace: "default", Pod: "web-1", Port: 80}, "", freePort(t)); err != nil {
		t.Fatal(err)
	}
	settle(t, p, func() bool { return len(ggui.Peek(m.Forwards)) == 0 }, "the forward to be dropped")
}

// Made up, as the screens are, the model forwards nothing and runs
// nothing against the cluster.
func TestOfflineStartsNothing(t *testing.T) {
	m, p, _ := kubeModel(t, true)
	called := false
	m.forwarder = func(context.Context, kubeapi.Target, net.Listener, func(error)) error { called = true; return nil }
	port := freePort(t)
	f, err := m.StartForward(kubeapi.Target{Namespace: "default", Pod: "web-1", Port: 80}, "", port)
	if err != nil || f.ID != 0 || len(ggui.Peek(m.Forwards)) != 0 {
		t.Errorf("offline start: %+v %v", f, err)
	}
	if _, err := dial(port); err == nil {
		t.Error("offline, a port was opened")
	}
	m.AskForwardPod(ownedPod("default", "web-1", kubeapi.KindDeployment, "web"))
	m.ForwardRemote.Set("80")
	m.ForwardLocal.Set(itoa(port))
	m.SubmitForward()
	m.ScaleWorkload(kubeapi.Workload{Kind: kubeapi.KindDeployment, Metadata: kubeapi.Meta{Namespace: "default", Name: "web"}}, 3)
	m.LoadPods()
	p.Frame()
	if called || m.podsLoading || len(ggui.Peek(m.Forwards)) != 0 {
		t.Errorf("offline: forwarder %v, loading %v", called, m.podsLoading)
	}
	if _, err := dial(port); err == nil {
		t.Error("offline, the dialog opened a port")
	}
}

// The Pods page is the cluster's surface: its views switch in place, the
// pods of one workload can be shown alone, and its Go-to entries follow
// the cluster as the Pods item does.
func TestTheKubernetesViews(t *testing.T) {
	m, p, k3s := kubeModel(t, true)
	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if n.Name == text {
				return true
			}
		}
		return false
	}
	m.Pods.Set(Listing[kubeapi.Pod]{Loaded: true, Items: sortPods([]kubeapi.Pod{
		ownedPod("default", "web-7d4b9c8f6d-a", kubeapi.KindDeployment, "web"),
		ownedPod("shop", "redis-0", kubeapi.KindStatefulSet, "redis"),
	})})
	web := kubeapi.Workload{Kind: kubeapi.KindDeployment, Metadata: kubeapi.Meta{Namespace: "default", Name: "web"}, Desired: 1, Ready: 1, UpToDate: 1}
	m.Workloads.Set(Listing[kubeapi.Workload]{Loaded: true, Items: []kubeapi.Workload{web,
		{Kind: kubeapi.KindDeployment, Metadata: kubeapi.Meta{Namespace: "kube-system", Name: "coredns"}, Desired: 1, Ready: 1, UpToDate: 1}}})
	m.Services.Set(Listing[kubeapi.Service]{Loaded: true, Items: []kubeapi.Service{{
		Metadata: kubeapi.Meta{Namespace: "default", Name: "web-svc"},
		Spec: kubeapi.ServiceSpec{Type: "NodePort", Selector: map[string]string{"app": "web"},
			Ports: []kubeapi.ServicePort{{Port: 80, NodePort: 30080, Protocol: "TCP"}}}}}})

	m.GoKube(KubeWorkloads)
	p.Frame()
	if ggui.Peek(m.Page) != PageWorkloads || !shows("Workloads") || !shows("web") || shows("coredns") {
		t.Errorf("the Workloads view:\n%s", p.Semantics())
	}
	m.GoKube(KubeServices)
	p.Frame()
	if !shows("web-svc") || !shows("30080 · not reachable") {
		t.Errorf("the Services view:\n%s", p.Semantics())
	}
	m.ShowWorkloadPods(web)
	p.Frame()
	if ggui.Peek(m.Page) != PagePods || !shows("web-7d4b9c8f6d-a") || shows("redis-0") || !shows("Show all pods") {
		t.Errorf("the pods of web:\n%s", p.Semantics())
	}
	if got := ggui.Peek(m.Pod).Name(); got != "web-7d4b9c8f6d-a" {
		t.Errorf("chosen %s", got)
	}
	m.PodOwner.Set(WorkloadRef{})
	p.Frame()
	if !shows("redis-0") {
		t.Errorf("all pods again:\n%s", p.Semantics())
	}

	// The palette's Workloads and Services entries are greyed out with the
	// Pods item.
	entry := func(name string) bool {
		for _, n := range p.Semantics().Nodes(ggui.RoleMenuItem) {
			if n.Name == name {
				return n.Disabled
			}
		}
		t.Fatalf("no %s entry:\n%s", name, p.Semantics())
		return false
	}
	m.PaletteOpen.Set(true)
	p.Frame()
	if entry("Workloads") || entry("Services") {
		t.Error("running: an entry is disabled")
	}
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Stopped}})
	p.Frame()
	if !entry("Workloads") || !entry("Services") {
		t.Error("stopped: an entry is enabled")
	}
}

func TestMenuListsForwards(t *testing.T) {
	k3s := Row{Spec: recipes.KubernetesNode(), State: sandbox.State{Status: sandbox.Running}}
	f := PortForward{ID: 1, Target: kubeapi.Target{Namespace: "default", Service: "web", Port: 80}, Local: 8080, Served: 3, Err: "x"}
	a := menuState([]Row{k3s}, Listing[dockerapi.Container]{}, Listing[kubeapi.Pod]{}, false, LinkView{}, LinkView{})
	a.Forwards = menuForwards([]PortForward{f})
	b := a
	f.Served, f.Err = 4, ""
	b.Forwards = menuForwards([]PortForward{f})
	if !a.Equal(b) {
		t.Error("a connection carried builds the menu again")
	}
	if a.Forwards[0].TargetText() != "svc/web:80" || a.Forwards[0].Address() != "127.0.0.1:8080" {
		t.Errorf("menu forward %+v", a.Forwards[0])
	}
	b.Forwards = nil
	if a.Equal(b) {
		t.Error("a forward stopped leaves the menu as it was")
	}
}
