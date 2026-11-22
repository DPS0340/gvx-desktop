package ui

import (
	"cmp"
	"image/color"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// The states of the Pods page, past the cluster's sandbox's: k3s is still
// coming up, the API server did not answer, it has yet to, or it answered;
// and of its Pods view: the cluster has no pods, it has only the system's
// and those are hidden, the workload chosen has none, or it has some to
// show.
const (
	podsWaiting = iota + 1
	podsFailed
	podsLoading
	podsAnswered
	podsNone
	podsOnlySystem
	podsNoneOwned
	podsListed
)

// podsPage is the app's view of the k3s cluster: its pods, as the
// Containers page lists the Docker Engine's containers, with the details
// of the chosen one under them; the workloads that keep them; and the
// services that lead to them, one view at a time, with the port forwards
// that run above each. It tells what to do while the cluster is not
// running.
func podsPage(m *Model) ggui.Widget {
	state := ggui.Derived(func() int {
		l := m.Pods.Get()
		switch {
		case m.PodsAccess.Get() != PodsOpen:
			return 0
		case m.PodsWaiting.Get():
			return podsWaiting
		case l.Err != "":
			return podsFailed
		case !l.Loaded:
			return podsLoading
		}
		return podsAnswered
	})
	body := ggui.View(state, func(s int) ggui.Widget {
		kube := ui.Button(tr("Open Kubernetes page"), func() { m.Go(PageKubernetes) }).Outline()
		switch s {
		case 0:
			stage := ggui.Map(m.PodsAccess, func(a PodsAccess) int { return int(a) })
			return ggui.View(gateOf(m, recipes.Kubernetes, stage), func(g engineGate) ggui.Widget { return engineNotRunning(m, g, kubeGate()) })
		case podsWaiting:
			return ui.Empty(tr("The cluster is starting…"), tr("Its pods show here once the API server answers. k3s takes up to two minutes to come up.")).
				Media(ui.Spinner().Size(28).Color(colorAmber)).Action(kube).Outline()
		case podsFailed:
			return ui.Alert(tr("Could not reach the API server"), ggui.Peek(m.Pods).Err).Destructive().Icon(icons.Alert)
		case podsLoading:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Asking the API server…"))).Gap(8)
		}
		return ui.Tabs(m.KubeView,
			ui.Tab(tr("Pods"), kubeView(m, podsView(m))),
			ui.Tab(tr("Workloads"), kubeView(m, workloadsView(m))),
			ui.Tab(tr("Services"), kubeView(m, servicesView(m))),
		).Name(tr("Cluster views"))
	})

	header := ggui.View(m.KubeView, func(v int) ggui.Widget {
		return pageHeader(kubeViewTitle(v), ui.CaptionOf(ggui.Derived(func() string { return kubeViewSummary(m, v) })),
			tip(iconButton(lucide.RefreshCw, tr("Refresh"), func() {
				m.LoadPods()
				m.LoadPodLogs()
			}).BindDisabled(ggui.Not(m.PodsOpen)), tr("Refresh")),
			ui.Button("Kubernetes", func() { m.Go(PageKubernetes) }).Outline().Icon(glyph(lucide.ShipWheel, 16)),
		)
	})
	return page(header, body)
}

// kubeViewTitle is the title of the page on a view.
func kubeViewTitle(v int) string {
	switch v {
	case KubeWorkloads:
		return tr("Workloads")
	case KubeServices:
		return tr("Services")
	}
	return tr("Pods")
}

// kubeViewSummary is the line under the page's title on a view: what it
// counts, once the cluster has answered.
func kubeViewSummary(m *Model, v int) string {
	answered := m.PodsOpen.Get() && m.Pods.Get().Loaded && m.Pods.Get().Err == ""
	switch v {
	case KubeWorkloads:
		l := m.Workloads.Get()
		if !answered || !l.Loaded || l.Err != "" {
			return tr("Deployments, StatefulSets, DaemonSets and Jobs make the cluster's pods and keep them running.")
		}
		return trn(len(l.Items), "%d workload in all. Deployments, StatefulSets, DaemonSets and Jobs make the cluster's pods and keep them running.",
			"%d workloads in all. Deployments, StatefulSets, DaemonSets and Jobs make the cluster's pods and keep them running.")
	case KubeServices:
		spec := m.clusterSpec()
		ranges := recipes.NodePortRanges(spec)
		var lb []string
		for _, p := range recipes.LoadBalancerPorts(spec) {
			lb = append(lb, itoa(p))
		}
		switch {
		case len(ranges) == 0 && len(lb) == 0:
			return tr("Services give pods one address in the cluster. No NodePorts reach this computer; forward a port instead.")
		case len(lb) == 0:
			return tr("Services give pods one address in the cluster. NodePorts %s reach this computer; forward a port for any other.", strings.Join(ranges, ", "))
		case len(ranges) == 0:
			return tr("Services give pods one address in the cluster. Load balancer ports %s reach this computer; forward a port for any other.", strings.Join(lb, ", "))
		}
		return tr("Services give pods one address in the cluster. NodePorts %s and load balancer ports %s reach this computer; forward a port for any other.",
			strings.Join(ranges, ", "), strings.Join(lb, ", "))
	}
	if l := m.Pods.Get(); answered {
		running, total, namespaces := podCounts(l.Items)
		return tr("%d running · %d total in %d namespaces. Pods run on the k3s node in the Kubernetes sandbox.", running, total, namespaces)
	}
	return tr("Pods run on the k3s node in the Kubernetes sandbox.")
}

// clusterSpec is the spec of the cluster's sandbox, whose ports say which
// NodePorts reach this computer.
func (m *Model) clusterSpec() sandbox.Spec {
	r, _ := m.RecipeSandbox(m.Rows.Get(), recipes.Kubernetes)
	return r.Spec
}

// kubeView is a view of the cluster under the port forwards that run.
func kubeView(m *Model, view ggui.Widget) ggui.Widget {
	return widget.Padding(widget.Column(forwardsCard(m), view).Stretch(), 12, 0, 0, 0)
}

// podsView is the Pods view: the table of pods and the chosen one's
// details, or why there is none.
func podsView(m *Model) ggui.Widget {
	state := ggui.Derived(func() int {
		l := m.Pods.Get()
		mine := userPods(l.Items, m.SystemPods.Get())
		switch {
		case len(l.Items) == 0:
			return podsNone
		case len(mine) == 0:
			return podsOnlySystem
		case len(ownedPods(mine, m.PodOwner.Get())) == 0:
			return podsNoneOwned
		}
		return podsListed
	})
	return ggui.View(state, func(s int) ggui.Widget {
		media := glyph(lucide.Hexagon, 32).Color(colorGray)
		switch s {
		case podsNone:
			return ui.Empty(tr("No pods"), tr("Run one with kubectl: kubectl create deployment web --image=nginx:alpine")).Media(media).Outline()
		case podsOnlySystem:
			return ui.Empty(tr("No pods of your own"), tr("Only the cluster's own pods run, in kube-system. Run one with kubectl: kubectl create deployment web --image=nginx:alpine")).
				Media(media).Action(ui.Button(tr("Show system pods"), func() { m.SystemPods.Set(true) }).Outline()).Outline()
		case podsNoneOwned:
			ref := ggui.Peek(m.PodOwner)
			return widget.Column(
				tableCard(podsTable(m)),
				ui.Empty(tr("%s %s has no pods", ref.Kind, ref.Name), tr("It has none running now, or they are in a namespace the System switch hides.")).
					Media(media).Action(ui.Button(tr("Show all pods"), func() { m.PodOwner.Set(WorkloadRef{}) }).Outline()).Outline(),
			).Gap(16).Stretch()
		}
		return widget.Column(
			tableCard(podsTable(m)),
			podDetail(m),
		).Gap(16).Stretch()
	})
}

// kubeGate words the Pods page while the cluster does not run.
func kubeGate() gateWords {
	return gateWords{
		recipe: recipes.Kubernetes, page: PageKubernetes, icon: lucide.Hexagon,
		openPage: tr("Open Kubernetes page"),
		notSetUp: [2]string{tr("Kubernetes is not set up"),
			tr("Set it up to run a single-node k3s cluster on this machine. Its pods show here while it runs.")},
		starting: [2]string{tr("Kubernetes is starting"), tr("Its pods show here once k3s is up, which takes up to two minutes.")},
		stopped:  [2]string{tr("Kubernetes is not running"), tr("Start it to see and manage its pods here.")},
		start:    tr("Start Kubernetes"),
	}
}

// podsTable is the cluster's pods, the system's only when asked for, those
// of one workload when one is chosen, filtered by the query.
func podsTable(m *Model) ggui.Widget {
	all := ggui.Derived(func() []kubeapi.Pod {
		return ownedPods(userPods(m.Pods.Get().Items, m.SystemPods.Get()), m.PodOwner.Get())
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.PodQuery, filterPods).WithEqual(nil)
	count := ggui.Combine(visible, all, func(v, a []kubeapi.Pod) string { return showing(len(v), len(a)) })
	selected := ggui.Bind(ggui.Map(m.Pod, kubeapi.Pod.Key).Get, m.SelectPod)
	// The workload whose pods alone show, with a way back to all of them.
	owner := ggui.View(m.PodOwner, func(ref WorkloadRef) ggui.Widget {
		if ref == (WorkloadRef{}) {
			return widget.Box()
		}
		return tip(ui.Button(ref.Kind+" "+ref.Name, func() { m.PodOwner.Set(WorkloadRef{}) }).Outline().Size(ui.ButtonExtraSmall).
			Icon(glyph(lucide.X, 12)).Name(tr("Show all pods")), tr("Show all pods"))
	})
	toolbar := widget.Row(
		widget.Expanded(filterBar(m.PodQuery, tr("Filter by name, namespace or status"), count)),
		owner,
		ui.Switch(m.SystemPods, tr("System pods")).Small(),
	).Gap(16).Align(widget.AlignCenter)
	return ui.Table(visible, kubeapi.Pod.Key,
		ui.Col(tr("Status"), func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return ggui.View(r, podStatusLabel)
		}).W(170),
		ui.Col(tr("Name"), func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, kubeapi.Pod.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, kubeapi.Pod.Namespace)).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).Grow(3),
		ui.Col(tr("Ready"), func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return widget.TextOf(ggui.Map(r, func(p kubeapi.Pod) string { return p.Summarize().ReadyText() })).Size(13).NoWrap()
		}).W(76),
		ui.Col(tr("Restarts"), func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return ggui.View(ggui.Map(r, kubeapi.Pod.Summarize), func(s kubeapi.Summary) ggui.Widget {
				n := widget.Text(itoa(s.Restarts)).Size(13).NoWrap()
				if s.Restarts == 0 || s.LastRestart.IsZero() {
					return n
				}
				last := s.LastRestart
				return widget.Column(n, ui.CaptionOf(ggui.Map(m.Now, func(now time.Time) string { return ago(last, now) })).Size(11).NoWrap()).Gap(1)
			})
		}).W(100),
		ui.Col(tr("CPU / Memory"), func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return usageCell(ggui.Combine(r, m.PodUsage, func(p kubeapi.Pod, all map[string]PodUse) usageView {
				u, ok := all[p.Key()]
				return podUsageOf(u.Total, ok)
			}))
		}).W(usageWidth),
		ui.Col(tr("Age"), func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(p kubeapi.Pod, now time.Time) string {
				if p.Metadata.Created.IsZero() {
					return "—"
				}
				return since(p.Metadata.Created, now)
			})).Size(12).NoWrap()
		}).W(120),
		ui.Col("", func(r ggui.Readable[kubeapi.Pod]) ggui.Widget {
			return ggui.View(r, func(p kubeapi.Pod) ggui.Widget {
				return widget.Row(
					tip(iconButton(lucide.ScrollText, tr("Logs of %s", p.Name()), func() { m.ShowPodLogs(p) }), tr("Logs")),
					podMenu(m, p),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(selected).RowName(kubeapi.Pod.Name).RowHeight(48).MaxRows(5).
		Toolbar(toolbar)
}

// healthColor is the color of a pod's health.
func healthColor(h kubeapi.Health) color.Color {
	switch h {
	case kubeapi.Healthy:
		return colorGreen
	case kubeapi.Changing:
		return colorAmber
	case kubeapi.Done:
		return colorBlue
	case kubeapi.Failing:
		return colorRed
	}
	return colorGray
}

// podStatusLabel is a pod's status in kubectl get pods' words, colored by
// what it means, with what the pod says of it in a tip.
func podStatusLabel(p kubeapi.Pod) ggui.Widget {
	s := p.Summarize()
	h := s.Health()
	changing := h == kubeapi.Changing && s.Status != "Running" && s.Status != "NotReady"
	return stateLabel(healthColor(h), s.Status, changing, p.Message())
}

// podMenu is what can be done to a pod.
func podMenu(m *Model, p kubeapi.Pod) ggui.Widget {
	link := ggui.Peek(m.KubeLink.View)
	ctr := podLogContainer(p, ggui.Peek(m.PodContainer))
	return moreMenu(tr("More actions for %s", p.Name()),
		ui.MenuItem(tr("Logs"), func() { m.ShowPodLogs(p) }),
		ui.MenuItem(tr("Copy name"), func() { m.Copy(p.Name(), p.Name()) }),
		ui.MenuItem(tr("Copy kubectl logs command"), func() { m.Copy(kubectlLogs(link, p, ctr), "kubectl logs") }),
		ui.MenuItem(tr("Copy kubectl exec command"), func() { m.Copy(kubectlExec(link, p, ctr), "kubectl exec") }),
		ui.MenuItem(tr("Copy kubectl describe command"), func() { m.Copy(kubectlDescribe(link, p), "kubectl describe") }),
		ui.MenuDivider(),
		ui.MenuItem(tr("Forward a port…"), func() { m.AskForwardPod(p) }).Disabled(!podForwardable(p)),
		ui.MenuDivider(),
		ui.MenuItem(tr("Delete…"), func() { m.AskDeletePod(p) }).Destructive(),
	)
}

// ---- Detail -------------------------------------------------------------

// podDetail is the chosen pod: its state and actions, and tabs with its
// logs and what it is made of.
func podDetail(m *Model) ggui.Widget {
	key := ggui.Map(m.Pod, kubeapi.Pod.Key)
	return entering(key, func(string) ggui.Widget {
		header := ggui.View(m.Pod, func(p kubeapi.Pod) ggui.Widget { return podHeader(m, p) })
		tabs := ui.Tabs(m.PodDetail,
			ui.Tab(tr("Logs"), podLogsView(m)),
			ui.Tab(tr("Info"), ggui.View(m.Pod, func(p kubeapi.Pod) ggui.Widget { return podInfo(m, p) })),
			ui.Tab(tr("Terminal"), podTerminal(m)),
		).Line().Name(tr("Pod details"))
		return ui.Card(widget.Column(header, tabs).Gap(12).Stretch())
	})
}

// workloadText names what keeps a pod, as kubectl names objects.
func workloadText(p kubeapi.Pod) string {
	if w, ok := p.Workload(); ok {
		return w.Kind + " " + w.Name
	}
	return ""
}

func podHeader(m *Model, p kubeapi.Pod) ggui.Widget {
	meta := ui.CaptionOf(ggui.Map(m.Now, func(now time.Time) string {
		parts := []string{p.Namespace()}
		if w := workloadText(p); w != "" {
			parts = append(parts, w)
		}
		if ip := p.Status.PodIP; ip != "" {
			parts = append(parts, ip)
		}
		if !p.Metadata.Created.IsZero() {
			parts = append(parts, tr("created %s", since(p.Metadata.Created, now)))
		}
		return strings.Join(parts, " · ")
	})).Size(12).NoWrap().Ellipsis()
	usage := usageLine(ggui.Map(m.PodUsage, func(all map[string]PodUse) usageView {
		u, ok := all[p.Key()]
		return podUsageOf(u.Total, ok)
	}))
	state := widget.Row(podStatusLabel(p), usage).Gap(16).Align(widget.AlignCenter)
	return detailHead(p.Name(), state, meta,
		ui.Button(tr("Forward a port…"), func() { m.AskForwardPod(p) }).Outline().Icon(glyph(lucide.ArrowLeftRight, 14)).Disabled(!podForwardable(p)),
		ui.Button(tr("Delete"), func() { m.AskDeletePod(p) }).Outline().Icon(glyph(lucide.Trash, 14)).Disabled(p.Metadata.Deleted != nil),
		podMenu(m, p),
	)
}

// podLogsView shows the last lines the chosen container of the chosen pod
// wrote, read again as often as the pods are, with a choice of its
// containers when it has more than one.
func podLogsView(m *Model) ggui.Widget {
	return engineLogsView(m, m.PodLogs,
		tr("The last %d lines, as kubectl logs shows them.", podLogTail), tr("The container has written nothing yet."),
		podContainerPicker(m, m.PodLogContainer.Get, false),
		tip(iconButton(lucide.RefreshCw, tr("Refresh logs"), m.LoadPodLogs), tr("Refresh")))
}

// podContainerPicker chooses a container of the chosen pod, when it has
// more than one, showing chosen as the one chosen; running leaves out
// those that do not run.
func podContainerPicker(m *Model, chosen func() string, running bool) ggui.Widget {
	names := ggui.Map(m.Pod, func(p kubeapi.Pod) []string {
		if !running {
			return p.ContainerNames()
		}
		return slices.DeleteFunc(p.ContainerNames(), func(n string) bool { return !podContainerRunning(p, n) })
	}).WithEqual(slices.Equal[[]string])
	return ggui.View(names, func(list []string) ggui.Widget {
		if len(list) < 2 {
			return widget.Box()
		}
		p := ggui.Peek(m.Pod)
		pick := ggui.Bind(chosen, m.PodContainer.Set)
		return ui.Select(pick).Options(list).Format(func(n string) string {
			if p.IsInit(n) {
				return tr("%s (init)", n)
			}
			return n
		}).Small().MinWidth(140).Name(tr("Container"))
	})
}

// podTerminal is a shell in a running container of the chosen pod, as
// kubectl exec -it runs one, with a choice of its running containers.
func podTerminal(m *Model) ggui.Widget {
	shellCtr := ggui.Combine(m.Pod, m.PodContainer, podShellContainer)
	copyCmd := func() {
		cmd := kubectlExec(ggui.Peek(m.KubeLink.View), ggui.Peek(m.Pod), ggui.Peek(shellCtr))
		m.Copy(cmd, "kubectl exec")
	}
	return terminalTab(m, m.podShell(), tr("A shell in the container, as kubectl exec -it runs one."),
		podContainerPicker(m, shellCtr.Get, true),
		tip(iconButton(lucide.Copy, tr("Copy kubectl exec command"), copyCmd), tr("Copy")))
}

// podInfo tells what the pod is made of and where it runs, and the
// commands that reach it.
func podInfo(m *Model, p kubeapi.Pod) ggui.Widget {
	parts := []ggui.Widget{sectionTitle(tr("Containers"), "")}
	for _, c := range append(slices.Clone(p.Spec.Containers), p.Spec.InitContainers...) {
		parts = append(parts, podContainerRow(m, p, c))
	}
	text := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	created := "—"
	if !p.Metadata.Created.IsZero() {
		created = p.Metadata.Created.Local().Format("2006-01-02 15:04:05")
	}
	parts = append(parts,
		sectionTitle(tr("Pod"), ""),
		kvText(tr("Namespace"), p.Namespace()),
		kvText(tr("Node"), text(p.Spec.Node)),
		kvText(tr("Pod IP"), text(p.Status.PodIP)),
		kvText(tr("Ports"), text(podPortsText(p))),
		kvText(tr("Created"), created),
		kvText(tr("Owner"), text(workloadText(p))),
	)
	if msg := p.Message(); msg != "" {
		parts = append(parts, kv(tr("Message"), widget.Text(msg).Size(13).Selectable()))
	}
	if len(p.Metadata.Labels) > 0 {
		var labels []ggui.Widget
		for _, k := range slices.Sorted(maps.Keys(p.Metadata.Labels)) {
			labels = append(labels, ui.Badge(k+"="+p.Metadata.Labels[k]).Outline())
		}
		parts = append(parts, kv(tr("Labels"), widget.Wrap(labels...).Gap(6)))
	}
	parts = append(parts, ggui.View(m.KubeLink.View, func(v LinkView) ggui.Widget {
		help := tr("For any terminal: each names the cluster's context or kubeconfig.")
		if v.C.InUse {
			help = tr("For a terminal where kubectl uses the gvx-k3s context.")
		}
		ctr := podLogContainer(p, "")
		return widget.Column(
			sectionTitle(tr("Commands"), help),
			copyable(m, kubectlLogs(v, p, ctr), "kubectl logs"),
			copyable(m, kubectlExec(v, p, ctr), "kubectl exec"),
			copyable(m, kubectlDescribe(v, p), "kubectl describe"),
		).Gap(10).Stretch()
	}))
	return widget.Padding(widget.Column(parts...).Gap(10).Stretch(), 8, 0)
}

// podPortsText lists the ports a pod's containers declare: 80/TCP (http).
func podPortsText(p kubeapi.Pod) string {
	var out []string
	for _, cp := range p.Ports() {
		s := itoa(cp.Port) + "/" + cmp.Or(cp.Protocol, "TCP")
		if cp.Name != "" {
			s += " (" + cp.Name + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// podContainerRow is a container of a pod: its state, name, image,
// restarts, and what it uses as metrics-server last sampled it.
func podContainerRow(m *Model, p kubeapi.Pod, c kubeapi.Container) ggui.Widget {
	st, ok := p.ContainerStatusOf(c.Name)
	col, state := colorGray, tr("Not created yet")
	if ok {
		state = st.StateText()
		switch {
		case st.State.Running != nil && st.Ready:
			col = colorGreen
		case st.State.Running != nil, st.State.Waiting != nil && !strings.Contains(st.State.Waiting.Reason, "BackOff") && !strings.HasPrefix(st.State.Waiting.Reason, "Err"):
			col = colorAmber
		case st.State.Terminated != nil && st.State.Terminated.ExitCode == 0:
			col = colorBlue
		default:
			col = colorRed
		}
	}
	name := c.Name
	if p.IsInit(c.Name) {
		name = tr("%s (init)", c.Name)
	}
	restarts := ""
	if ok {
		restarts = tr("Restarts: %d", st.Restarts)
	}
	return widget.Row(
		widget.Box(dot(col, 8)).Pad(2),
		widget.Expanded(widget.Column(
			widget.Text(name).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			ui.Mono(c.Image).Size(11).NoWrap().Ellipsis(),
		).Gap(2)),
		widget.Box(ui.Caption(state).Size(12).NoWrap().Ellipsis()).Width(200),
		widget.Box(ui.Caption(restarts).Size(12).NoWrap()).Width(90),
		widget.Box(usageCell(ggui.Map(m.PodUsage, func(all map[string]PodUse) usageView {
			u, ok := all[p.Key()].Containers[c.Name]
			return podUsageOf(u, ok)
		}))).Width(usageWidth),
	).Gap(10).Align(widget.AlignCenter)
}

// podsSummary counts the cluster's pods, workloads, services and the port
// forwards that run, and leads to the Pods page, which lists them.
func podsSummary(m *Model) ggui.Widget {
	line := ggui.Derived(func() [3]string {
		a, l := m.PodsAccess.Get(), m.Pods.Get()
		switch {
		case a != PodsOpen:
			return [3]string{a.Reason()}
		case m.PodsWaiting.Get():
			return [3]string{tr("The cluster is starting…"), tr("Its pods show here once the API server answers. k3s takes up to two minutes to come up.")}
		case l.Err != "":
			return [3]string{tr("Could not reach the API server"), l.Err}
		case !l.Loaded:
			return [3]string{tr("Asking the API server…")}
		}
		running, total, _ := podCounts(l.Items)
		var mine []string
		for _, p := range userPods(l.Items, false) {
			mine = append(mine, p.Name())
		}
		return [3]string{tr("%d running · %d total", running, total), clusterCounts(m.Workloads.Get(), m.Services.Get(), m.Forwards.Get()), strings.Join(mine, ", ")}
	})
	text := ggui.View(line, func(t [3]string) ggui.Widget {
		col := []ggui.Widget{widget.Text(t[0]).Size(13).Weight(ggui.WeightMedium)}
		for _, s := range t[1:] {
			if s != "" {
				col = append(col, ui.Caption(s).Size(12).NoWrap().Ellipsis())
			}
		}
		return widget.Column(col...).Gap(2)
	})
	return ui.Card(widget.Row(
		glyph(lucide.Hexagon, 20).Color(colorGray),
		widget.Expanded(text),
		ui.Button(tr("Open Pods"), func() { m.GoKube(KubePods) }).Outline().Size(ui.ButtonSmall).
			Icon(glyph(lucide.ArrowRight, 14)).BindDisabled(ggui.Not(m.PodsOpen)),
	).Gap(14).Align(widget.AlignCenter))
}

// clusterCounts counts in a line the workloads and services, as far as
// they were read, and the port forwards that run, with where they listen.
func clusterCounts(wl Listing[kubeapi.Workload], sv Listing[kubeapi.Service], fw []PortForward) string {
	var parts []string
	if wl.Loaded && wl.Err == "" {
		parts = append(parts, trn(len(wl.Items), "%d workload", "%d workloads"))
	}
	if sv.Loaded && sv.Err == "" {
		parts = append(parts, trn(len(sv.Items), "%d service", "%d services"))
	}
	if len(fw) > 0 {
		var at []string
		for _, f := range fw {
			at = append(at, f.Address()+" → "+f.TargetText())
		}
		parts = append(parts, trn(len(fw), "%d port forward", "%d port forwards")+": "+strings.Join(at, ", "))
	}
	return strings.Join(parts, " · ")
}
