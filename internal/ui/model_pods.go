package ui

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// PodsAccess is whether the Pods page can be used, and if not, why: its
// pods are the k3s cluster's, which answers only while its sandbox runs.
type PodsAccess int

const (
	PodsOpen      PodsAccess = iota // the cluster's sandbox is running
	PodsNoCluster                   // the Kubernetes recipe is not set up
	PodsStarting                    // the cluster's sandbox runs and k3s is not up yet
	PodsStopped                     // the cluster is stopped, stopping, exited or failed
)

// podsAccess reads it from the sandboxes.
func podsAccess(rows []Row) PodsAccess {
	return PodsAccess(engineStage(rows, recipes.Kubernetes))
}

// Reason says why the page cannot be used, or nothing when it can.
func (a PodsAccess) Reason() string {
	switch a {
	case PodsNoCluster:
		return tr("Set up Kubernetes to manage its pods")
	case PodsStarting:
		return tr("Kubernetes is starting")
	case PodsStopped:
		return tr("Start Kubernetes to manage its pods")
	}
	return ""
}

// podsWarmUp is how long after the cluster's sandbox starts a failure to
// reach its API server is taken for k3s still coming up, not an error.
const podsWarmUp = 3 * time.Minute

// systemNamespace reports whether a namespace is Kubernetes' own, where
// k3s runs coredns, the local-path provisioner and metrics-server.
func systemNamespace(ns string) bool { return strings.HasPrefix(ns, "kube-") }

// sortPods puts the pods of the user's namespaces first, then those of the
// system's, each by namespace and name.
func sortPods(list []kubeapi.Pod) []kubeapi.Pod {
	list = slices.Clone(list)
	slices.SortStableFunc(list, func(a, b kubeapi.Pod) int {
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

// userPods leaves out the system's pods, unless system says to keep them.
func userPods(list []kubeapi.Pod, system bool) []kubeapi.Pod {
	if system {
		return list
	}
	var out []kubeapi.Pod
	for _, p := range list {
		if !systemNamespace(p.Namespace()) {
			out = append(out, p)
		}
	}
	return out
}

// filterPods keeps the pods whose name, namespace or status has q in it,
// in either case.
func filterPods(list []kubeapi.Pod, q string) []kubeapi.Pod {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return list
	}
	var out []kubeapi.Pod
	for _, p := range list {
		if strings.Contains(strings.ToLower(p.Name()+" "+p.Namespace()+" "+p.Summarize().Status), q) {
			out = append(out, p)
		}
	}
	return out
}

// chosenPod is the pod with key, or the first when none has it.
func chosenPod(list []kubeapi.Pod, key string) kubeapi.Pod {
	for _, p := range list {
		if p.Key() == key {
			return p
		}
	}
	if len(list) > 0 {
		return list[0]
	}
	return kubeapi.Pod{}
}

// podsEqual compares listings of pods for signals.
func podsEqual(a, b Listing[kubeapi.Pod]) bool {
	return a.Err == b.Err && a.Loaded == b.Loaded && a.Loading == b.Loading && slices.EqualFunc(a.Items, b.Items, kubeapi.Pod.Equal)
}

// podCounts are the running pods, as kubectl get pods says Running, and
// all of them, and the namespaces they are in.
func podCounts(list []kubeapi.Pod) (running, total, namespaces int) {
	seen := map[string]bool{}
	for _, p := range list {
		if p.Summarize().Status == "Running" {
			running++
		}
		seen[p.Namespace()] = true
	}
	return running, len(list), len(seen)
}

// SelectPod shows a pod in the details under the table.
func (m *Model) SelectPod(key string) {
	if ggui.Peek(m.SelectedPod) == key {
		return
	}
	m.SelectedPod.Set(key)
	m.PodContainer.Set("")
}

// ShowPodLogs opens the Pods page on a pod's logs.
func (m *Model) ShowPodLogs(p kubeapi.Pod) {
	if systemNamespace(p.Namespace()) {
		m.SystemPods.Set(true)
	}
	m.SelectPod(p.Key())
	m.PodDetail.Set(0)
	m.Go(PagePods)
}

// podLogContainer is the container of p whose logs show: the one chosen,
// or the first.
func podLogContainer(p kubeapi.Pod, chosen string) string {
	names := p.ContainerNames()
	if slices.Contains(names, chosen) {
		return chosen
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// podLogsID is what PodLogs are of: a container of a pod.
func podLogsID(p kubeapi.Pod, container string) string {
	if p.Name() == "" || container == "" {
		return ""
	}
	return p.Key() + "/" + container
}

// podLogTail is how many of its last lines a container's logs show.
const podLogTail = containerLogTail

// podLogLines makes the kubelet's lines into the log viewer's, numbered
// by their time as a container's are.
func podLogLines(lines []kubeapi.LogLine) []sandbox.LogLine {
	return timedLines(len(lines), func(i int) (time.Time, string, bool) { return lines[i].Time, lines[i].Text, false })
}

// LoadPods lists the pods of the k3s cluster, and with them its workloads
// and services while a page shows them (see wantsDetails), in one go.
// Asked for while a listing runs, it lists them again after it. Until the
// cluster's API server has answered once, in its first minutes, a failure
// is the cluster still starting: the page waits rather than tells of an
// error.
func (m *Model) LoadPods() {
	if m.Offline {
		return // the pods shown are made up
	}
	row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Kubernetes)
	if !ok || row.State.Status != sandbox.Running {
		m.clearCluster()
		m.PodsWaiting.Set(false)
		m.podsAnswered = false
		return
	}
	if m.podsLoading {
		m.podsAgain = true
		return
	}
	m.podsLoading = true
	started := row.State.StartedAt
	file := recipes.Kubeconfig()
	workloads, services := m.wantsDetails()
	metrics := ggui.Untrack(m.wantsPodUsage)
	var got clusterList
	noConfig := false
	m.async(func(ctx context.Context) error {
		if _, err := os.Stat(file); err != nil {
			noConfig = true
			return nil
		}
		c, err := kubeapi.Open(file)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		got = listCluster(ctx, c, workloads, services, metrics)
		return got.podsErr
	}, func(err error) {
		m.podsLoading = false
		if m.podsAgain {
			m.podsAgain = false
			m.LoadPods()
			return
		}
		if row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Kubernetes); !ok || row.State.Status != sandbox.Running {
			return // the cluster stopped meanwhile, and its list was emptied
		}
		m.KubeconfigReady.Set(!noConfig)
		warming := !m.podsAnswered && (started.IsZero() || time.Since(started) < podsWarmUp)
		if noConfig || err != nil && warming {
			m.PodsWaiting.Set(true)
			m.clearCluster()
			return
		}
		m.PodsWaiting.Set(false)
		l := Listing[kubeapi.Pod]{Items: sortPods(got.pods), Loaded: true}
		if err != nil {
			l.Err = err.Error()
		} else {
			m.podsAnswered = true
			m.setDetails(got, workloads, services, metrics)
		}
		m.Pods.Set(l)
		if err == nil {
			m.checkForwards(got.pods, got.services, services && got.svErr == nil)
		}
	})
}

// LoadPodLogs reads the last lines of the chosen container of the chosen
// pod. Asked for while a reading runs, it reads them again after it.
func (m *Model) LoadPodLogs() {
	if m.Offline {
		return // the lines shown are made up
	}
	pod, ctr := ggui.Peek(m.Pod), ggui.Peek(m.PodLogContainer)
	id := podLogsID(pod, ctr)
	if id == "" || ggui.Peek(m.PodsAccess) != PodsOpen {
		m.PodLogs.Set(ContainerLogs{})
		return
	}
	if m.podLogsLoading {
		m.podLogsAgain = true
		return
	}
	m.podLogsLoading = true
	if ggui.Peek(m.PodLogs).ID != id {
		m.PodLogs.Set(ContainerLogs{ID: id})
	}
	file := recipes.Kubeconfig()
	var lines []kubeapi.LogLine
	m.async(func(ctx context.Context) error {
		c, err := kubeapi.Open(file)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		lines, err = c.Logs(ctx, pod.Namespace(), pod.Name(), ctr, podLogTail)
		return err
	}, func(err error) {
		m.podLogsLoading = false
		if m.podLogsAgain {
			m.podLogsAgain = false
			m.LoadPodLogs()
			return
		}
		if podLogsID(ggui.Peek(m.Pod), ggui.Peek(m.PodLogContainer)) != id {
			return
		}
		l := ContainerLogs{ID: id, Lines: podLogLines(lines), Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.PodLogs.Set(l)
	})
}

// followsPodLogs reports whether the chosen pod's logs are on screen, and
// are to be read as often as the pods are.
func (m *Model) followsPodLogs() bool {
	return m.Page.Get() == PagePods && m.PodDetail.Get() == 0 && m.Pod.Get().Name() != ""
}

// followPods keeps the cluster's pods current while it runs, for the Pods
// and Kubernetes pages and the menus, as followDocker does the engine's
// containers. Call it from Start.
func (m *Model) followPods() {
	if m.Offline {
		return
	}
	every := ggui.Derived(func() time.Duration { return m.recipePageInterval(recipes.Kubernetes, PagePods, PageKubernetes) })
	logs := ggui.Derived(m.followsPodLogs)
	m.poll(every, m.LoadPods, func() {
		m.LoadPods()
		if ggui.Peek(logs) {
			m.LoadPodLogs()
		}
	})
	id := ggui.Combine(m.Pod, m.PodLogContainer, podLogsID)
	shown := ggui.Combine(logs, id, func(on bool, id string) string {
		if !on {
			return ""
		}
		return id
	})
	m.followLogs(shown, id, m.LoadPodLogs, m.PodLogs)
}

// DeletePod deletes a pod, and lists the pods again.
func (m *Model) DeletePod(p kubeapi.Pod) {
	file := recipes.Kubeconfig()
	m.async(func(ctx context.Context) error {
		c, err := kubeapi.Open(file)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return c.DeletePod(ctx, p.Namespace(), p.Name())
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not delete %s", p.Name()), err)
		}
		m.LoadPods()
	})
}

// AskDeletePod confirms and deletes a pod, saying whether something
// starts another in its place.
func (m *Model) AskDeletePod(p kubeapi.Pod) {
	m.Confirm(tr("Delete pod %s?", p.Name()), podDeleteText(p), tr("Delete pod"), func() { m.DeletePod(p) })
}

// podDeleteText tells what deleting a pod leads to: a pod a workload
// keeps comes back, under a new name unless a StatefulSet's; one of a Job
// only while the Job has yet to finish; one of its own is gone.
func podDeleteText(p kubeapi.Pod) string {
	w, ok := p.Workload()
	switch {
	case !ok:
		return tr("Nothing owns it, so nothing starts it again.")
	case w.Kind == "Job":
		return tr("Its Job %s starts another only if it has not finished.", w.Name)
	}
	return tr("Its %s %s starts a new one in its place.", w.Kind, w.Name)
}

// kubectlCommand is a kubectl command line that reaches the cluster from
// a terminal: as it is when kubectl uses the gvx-k3s context, naming the
// context when kubectl has it and uses another, and the cluster's own
// kubeconfig when it has none.
func kubectlCommand(v LinkView, args ...string) string {
	cmd := []string{"kubectl"}
	switch {
	case v.C.InUse:
	case v.C.Exists:
		cmd = append(cmd, "--context", KubeContextName)
	default:
		cmd = append(cmd, "--kubeconfig", rungv.Quote(recipes.Kubeconfig()))
	}
	return strings.Join(append(cmd, args...), " ")
}

// The commands the Pods page copies, for a container of a pod.
func kubectlLogs(v LinkView, p kubeapi.Pod, ctr string) string {
	return kubectlCommand(v, "logs", "-f", "-n", p.Namespace(), p.Name(), "-c", ctr)
}

func kubectlExec(v LinkView, p kubeapi.Pod, ctr string) string {
	return kubectlCommand(v, "exec", "-it", "-n", p.Namespace(), p.Name(), "-c", ctr, "--", "sh")
}

func kubectlDescribe(v LinkView, p kubeapi.Pod) string {
	return kubectlCommand(v, "describe", "pod", "-n", p.Namespace(), p.Name())
}
