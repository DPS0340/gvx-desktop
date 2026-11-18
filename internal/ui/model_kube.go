package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// The Pods page is the app's view of the cluster: its pods, the workloads
// that keep them, and the services that lead to them, one at a time, as
// KubeView chooses.
const (
	KubePods = iota
	KubeWorkloads
	KubeServices
)

// WorkloadRef names a workload: the Pods view shows only its pods while
// one is set.
type WorkloadRef struct{ Kind, Namespace, Name string }

// workload is the workload the ref names, as far as Owns reads one.
func (r WorkloadRef) workload() kubeapi.Workload {
	return kubeapi.Workload{Kind: r.Kind, Metadata: kubeapi.Meta{Namespace: r.Namespace, Name: r.Name}}
}

func refOf(w kubeapi.Workload) WorkloadRef { return WorkloadRef{w.Kind, w.Namespace(), w.Name()} }

// ownedPods keeps the pods of the workload ref names, or all of them for
// none.
func ownedPods(list []kubeapi.Pod, ref WorkloadRef) []kubeapi.Pod {
	if ref == (WorkloadRef{}) {
		return list
	}
	w := ref.workload()
	var out []kubeapi.Pod
	for _, p := range list {
		if w.Owns(p) {
			out = append(out, p)
		}
	}
	return out
}

// GoKube opens the Pods page on one of its views.
func (m *Model) GoKube(view int) {
	m.KubeView.Set(view)
	m.Go(PagePods)
}

// ShowWorkloadPods opens the Pods view on the pods of a workload.
func (m *Model) ShowWorkloadPods(w kubeapi.Workload) {
	if systemNamespace(w.Namespace()) {
		m.SystemPods.Set(true)
	}
	m.PodOwner.Set(refOf(w))
	m.PodQuery.Set("")
	m.KubeView.Set(KubePods)
	m.Go(PagePods)
}

// ---- Workloads ----------------------------------------------------------

// kindOrder puts the kinds as kubectl get all lists them.
func kindOrder(kind string) int {
	return slices.Index([]string{kubeapi.KindDeployment, kubeapi.KindStatefulSet, kubeapi.KindDaemonSet, kubeapi.KindJob}, kind)
}

// sortWorkloads puts the user's namespaces first, then the system's, each
// by namespace, name and kind.
func sortWorkloads(list []kubeapi.Workload) []kubeapi.Workload {
	list = slices.Clone(list)
	slices.SortStableFunc(list, func(a, b kubeapi.Workload) int {
		if sa, sb := systemNamespace(a.Namespace()), systemNamespace(b.Namespace()); sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		if c := strings.Compare(a.Namespace(), b.Namespace()); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name(), b.Name()); c != 0 {
			return c
		}
		return kindOrder(a.Kind) - kindOrder(b.Kind)
	})
	return list
}

// userWorkloads leaves out the system's workloads unless system says to
// keep them, as the pods' switch does.
func userWorkloads(list []kubeapi.Workload, system bool) []kubeapi.Workload {
	if system {
		return list
	}
	return slices.DeleteFunc(slices.Clone(list), func(w kubeapi.Workload) bool { return systemNamespace(w.Namespace()) })
}

// filterWorkloads keeps the workloads whose kind, name, namespace or image
// has q in it, in either case.
func filterWorkloads(list []kubeapi.Workload, q string) []kubeapi.Workload {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return list
	}
	var out []kubeapi.Workload
	for _, w := range list {
		if strings.Contains(strings.ToLower(w.Kind+" "+w.Name()+" "+w.Namespace()+" "+strings.Join(w.Images, " ")), q) {
			out = append(out, w)
		}
	}
	return out
}

func workloadsEqual(a, b Listing[kubeapi.Workload]) bool {
	return a.Err == b.Err && a.Loaded == b.Loaded && slices.EqualFunc(a.Items, b.Items, kubeapi.Workload.Equal)
}

// workloadHealth is a workload's health as its counts tell it, made
// Failing by a pod of it that fails: a Deployment whose new pod crashes
// waits on it as long as it is let, and its counts say only "changing".
// why is what to say of it in a tip.
func workloadHealth(w kubeapi.Workload, pods []kubeapi.Pod) (h kubeapi.Health, why string) {
	h = w.Health()
	switch {
	case w.Metadata.Deleted != nil:
		return h, tr("Deleting, with its pods…")
	case h == kubeapi.Done:
		return h, tr("Complete")
	case h == kubeapi.Failing && w.Kind == kubeapi.KindJob:
		return h, trn(w.Failed, "Failed: %d pod failed", "Failed: %d pods failed")
	case h == kubeapi.Unknown:
		return h, tr("Scaled to 0")
	}
	for _, p := range pods {
		if s := p.Summarize(); w.Owns(p) && s.Health() == kubeapi.Failing {
			return kubeapi.Failing, p.Name() + ": " + s.Status
		}
	}
	switch {
	case h == kubeapi.Healthy:
		return h, tr("All %d ready", w.Desired)
	case w.Kind == kubeapi.KindJob:
		return h, trn(w.Active, "Running: %d pod active", "Running: %d pods active")
	case w.Rolling || w.UpToDate < w.Desired:
		return h, tr("Rolling out: %d of %d up to date", w.UpToDate, w.Desired)
	}
	return h, tr("%d of %d ready", w.Ready, w.Desired)
}

// kubectlWorkload is a kubectl command line on a workload, kind/name.
func kubectlWorkload(v LinkView, w kubeapi.Workload, verb string, more ...string) string {
	args := append([]string{verb, strings.ToLower(w.Kind) + "/" + w.Name(), "-n", w.Namespace()}, more...)
	return kubectlCommand(v, args...)
}

// ScaleWorkload asks for n replicas of a workload, and lists again.
func (m *Model) ScaleWorkload(w kubeapi.Workload, n int) {
	m.kubeDo(tr("Could not scale %s", w.Name()), func(ctx context.Context, c *kubeapi.Client) error {
		return c.Scale(ctx, w.Kind, w.Namespace(), w.Name(), n)
	})
}

// AskScale opens the dialog that scales a workload.
func (m *Model) AskScale(w kubeapi.Workload) {
	m.ScaleTarget.Set(w)
	m.ScaleReplicas.Set(float64(w.Desired))
	m.ScaleOpen.Set(true)
}

// AskRestart confirms and restarts a workload's pods, as kubectl rollout
// restart does.
func (m *Model) AskRestart(w kubeapi.Workload) {
	m.Confirm(tr("Restart %s %s?", w.Kind, w.Name()),
		tr("Its pods are replaced from its template, a few at a time, as kubectl rollout restart does."),
		tr("Restart"), func() {
			m.kubeDo(tr("Could not restart %s", w.Name()), func(ctx context.Context, c *kubeapi.Client) error {
				return c.Restart(ctx, w.Kind, w.Namespace(), w.Name(), time.Now())
			})
		})
}

// AskDeleteWorkload confirms and deletes a workload, with its pods.
func (m *Model) AskDeleteWorkload(w kubeapi.Workload) {
	n := len(ownedPods(ggui.Peek(m.Pods).Items, refOf(w)))
	m.Confirm(tr("Delete %s %s?", w.Kind, w.Name()), workloadDeleteText(w, n), tr("Delete"), func() {
		m.kubeDo(tr("Could not delete %s", w.Name()), func(ctx context.Context, c *kubeapi.Client) error {
			return c.DeleteWorkload(ctx, w.Kind, w.Namespace(), w.Name())
		})
	})
}

// workloadDeleteText tells what deleting a workload of n pods takes with
// it.
func workloadDeleteText(w kubeapi.Workload, n int) string {
	if n == 0 {
		return tr("It has no pods now; nothing will start them again.")
	}
	return trn(n, "Its %d pod is deleted with it, and nothing starts it again.", "Its %d pods are deleted with it, and nothing starts them again.")
}

// kubeDo runs work with a client of the cluster, says what failed under
// title, and lists the cluster again.
func (m *Model) kubeDo(title string, work func(ctx context.Context, c *kubeapi.Client) error) {
	if m.Offline {
		return
	}
	file := recipes.Kubeconfig()
	m.async(func(ctx context.Context) error {
		c, err := kubeapi.Open(file)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return work(ctx, c)
	}, func(err error) {
		if err != nil {
			m.toastError(title, err)
		}
		m.LoadPods()
	})
}

// ---- Services -----------------------------------------------------------

// sortServices orders services as sortPods orders pods.
func sortServices(list []kubeapi.Service) []kubeapi.Service {
	list = slices.Clone(list)
	slices.SortStableFunc(list, func(a, b kubeapi.Service) int {
		if sa, sb := systemNamespace(a.Namespace()), systemNamespace(b.Namespace()); sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		if c := strings.Compare(a.Namespace(), b.Namespace()); c != 0 {
			return c
		}
		return strings.Compare(a.Name(), b.Name())
	})
	return list
}

func userServices(list []kubeapi.Service, system bool) []kubeapi.Service {
	if system {
		return list
	}
	return slices.DeleteFunc(slices.Clone(list), func(s kubeapi.Service) bool { return systemNamespace(s.Namespace()) })
}

// filterServices keeps the services whose name, namespace, type or
// selector has q in it, in either case.
func filterServices(list []kubeapi.Service, q string) []kubeapi.Service {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return list
	}
	var out []kubeapi.Service
	for _, s := range list {
		if strings.Contains(strings.ToLower(s.Name()+" "+s.Namespace()+" "+s.Spec.Type+" "+s.SelectorText()), q) {
			out = append(out, s)
		}
	}
	return out
}

func servicesEqual(a, b Listing[kubeapi.Service]) bool {
	return a.Err == b.Err && a.Loaded == b.Loaded && slices.EqualFunc(a.Items, b.Items, kubeapi.Service.Equal)
}

// portText is a port of a service as kubectl describe pairs them: the
// port, the target port it leads to, and its protocol, 80→http/TCP.
func portText(p kubeapi.ServicePort) string {
	proto := p.Protocol
	if proto == "" {
		proto = "TCP"
	}
	target := p.TargetPort.String()
	if p.TargetPort == (kubeapi.IntOrString{}) {
		target = itoa(p.Port)
	}
	return fmt.Sprintf("%d→%s/%s", p.Port, target, proto)
}

// NodePortReach is whether a NodePort reaches this computer, and where.
type NodePortReach struct {
	Port    int
	Address string // host:port on this computer, when it reaches it
	Why     string // why it does not, when it does not
}

// nodePortReach tells of each NodePort of a service whether the cluster's
// sandbox publishes it, and so whether this computer reaches it.
func nodePortReach(s kubeapi.Service, spec sandbox.Spec) []NodePortReach {
	var out []NodePortReach
	for _, p := range s.Spec.Ports {
		if p.NodePort == 0 || p.Protocol != "" && p.Protocol != "TCP" {
			continue
		}
		r := NodePortReach{Port: p.NodePort}
		if addr, ok := recipes.NodePortAddress(spec, p.NodePort); ok {
			r.Address = addr
		} else {
			r.Why = nodePortWhy(p.NodePort, recipes.NodePortRanges(spec))
		}
		out = append(out, r)
	}
	return out
}

// nodePortWhy says why a NodePort does not reach this computer.
func nodePortWhy(port int, ranges []string) string {
	if len(ranges) == 0 {
		return tr("The Kubernetes sandbox publishes no NodePorts, so %d does not reach this computer. Add a range to its ports in the sandbox settings, or forward a port.", port)
	}
	return tr("The Kubernetes sandbox publishes only NodePorts %s to this computer, and %d is not among them. Widen the range in the sandbox settings, or forward a port.",
		strings.Join(ranges, ", "), port)
}

// forwardable says why a service cannot have a port forwarded, or nothing
// when it can: one with no selector has no pods to carry it to.
func forwardable(s kubeapi.Service) string {
	switch {
	case s.Spec.Type == "ExternalName":
		return tr("An ExternalName service names a host outside the cluster; it has no pods.")
	case len(s.Spec.Selector) == 0:
		return tr("It has no selector, so no pods a port could be forwarded to.")
	}
	return ""
}

// ---- Loading --------------------------------------------------------------

// wantsDetails reports whether the cluster's workloads and services are
// read with its pods, as they are while a page shows them; services are
// also read while a port forward runs to one, which ends when it goes.
func (m *Model) wantsDetails() (workloads, services bool) {
	page := ggui.Peek(m.Page)
	shown := page == PagePods || page == PageKubernetes
	return shown, shown || slices.ContainsFunc(ggui.Peek(m.Forwards), func(f PortForward) bool { return f.Target.Service != "" })
}

// clusterList is what one refresh of the cluster reads.
type clusterList struct {
	pods                  []kubeapi.Pod
	workloads             []kubeapi.Workload
	services              []kubeapi.Service
	metrics               []kubeapi.PodMetrics
	podsErr, wlErr, svErr error
	metricsErr            error
}

// listCluster reads the pods, and the workloads, services and what the
// pods use when asked to, all at once.
func listCluster(ctx context.Context, c *kubeapi.Client, workloads, services, metrics bool) clusterList {
	var l clusterList
	var wg sync.WaitGroup
	if workloads {
		wg.Go(func() { l.workloads, l.wlErr = c.Workloads(ctx) })
	}
	if services {
		wg.Go(func() { l.services, l.svErr = c.Services(ctx) })
	}
	if metrics {
		wg.Go(func() { l.metrics, l.metricsErr = c.PodMetrics(ctx) })
	}
	l.pods, l.podsErr = c.Pods(ctx)
	wg.Wait()
	return l
}

// setDetails keeps what a refresh read of the workloads and services, and
// of what the pods use. Without metrics-server, or before it has its
// first samples, they use what no one knows: the page shows a dash.
func (m *Model) setDetails(l clusterList, workloads, services, metrics bool) {
	if metrics {
		m.PodUsage.Set(podUses(l.metrics))
	}
	if workloads {
		wl := Listing[kubeapi.Workload]{Items: sortWorkloads(l.workloads), Loaded: true}
		if l.wlErr != nil {
			wl.Err = l.wlErr.Error()
		}
		m.Workloads.Set(wl)
	}
	if services {
		sv := Listing[kubeapi.Service]{Items: sortServices(l.services), Loaded: true}
		if l.svErr != nil {
			sv.Err = l.svErr.Error()
		}
		m.Services.Set(sv)
	}
}

// clearCluster forgets what was read of the cluster, as it stops.
func (m *Model) clearCluster() {
	m.Pods.Set(Listing[kubeapi.Pod]{})
	m.Workloads.Set(Listing[kubeapi.Workload]{})
	m.Services.Set(Listing[kubeapi.Service]{})
	m.PodUsage.Set(nil)
}
