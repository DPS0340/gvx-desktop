package ui

import (
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
)

// The states of the Pods page, past the cluster's sandbox's: k3s is still
// coming up, the API server did not answer, it has yet to, it has no pods,
// it has only the system's and those are hidden, or it has some to show.
const (
	podsWaiting = iota + 1
	podsFailed
	podsLoading
	podsNone
	podsOnlySystem
	podsListed
)

// podsPage lists the k3s cluster's pods as the Containers page lists the
// Docker Engine's containers: a table to filter, and the details of the
// chosen one under it. It tells what to do while the cluster is not
// running.
func podsPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Combine(m.PodsOpen, m.Pods, func(open bool, l Listing[kubeapi.Pod]) string {
		if !open || !l.Loaded || l.Err != "" {
			return tr("Pods run on the k3s node in the Kubernetes sandbox.")
		}
		running, total, namespaces := podCounts(l.Items)
		return tr("%d running · %d total in %d namespaces. Pods run on the k3s node in the Kubernetes sandbox.", running, total, namespaces)
	}))

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
		case len(l.Items) == 0:
			return podsNone
		case len(userPods(l.Items, m.SystemPods.Get())) == 0:
			return podsOnlySystem
		}
		return podsListed
	})
	body := ggui.View(state, func(s int) ggui.Widget {
		media := glyph(lucide.Hexagon, 32).Color(colorGray)
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
		case podsNone:
			return ui.Empty(tr("No pods"), tr("Run one with kubectl: kubectl create deployment web --image=nginx:alpine")).Media(media).Outline()
		case podsOnlySystem:
			return ui.Empty(tr("No pods of your own"), tr("Only the cluster's own pods run, in kube-system. Run one with kubectl: kubectl create deployment web --image=nginx:alpine")).
				Media(media).Action(ui.Button(tr("Show system pods"), func() { m.SystemPods.Set(true) }).Outline()).Outline()
		}
		return widget.Column(
			tableCard(podsTable(m)),
			podDetail(m),
		).Gap(16).Stretch()
	})

	return page(
		pageHeader(tr("Pods"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh pods"), func() {
				m.LoadPods()
				m.LoadPodLogs()
			}).BindDisabled(ggui.Not(m.PodsOpen)), tr("Refresh")),
			ui.Button("Kubernetes", func() { m.Go(PageKubernetes) }).Outline().Icon(glyph(lucide.ShipWheel, 16)),
		),
		body,
	)
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

// podsTable is the cluster's pods, the system's only when asked for,
// filtered by the query.
func podsTable(m *Model) ggui.Widget {
	all := ggui.Combine(m.Pods, m.SystemPods, func(l Listing[kubeapi.Pod], system bool) []kubeapi.Pod {
		return userPods(l.Items, system)
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.PodQuery, filterPods).WithEqual(nil)
	count := ggui.Combine(visible, all, func(v, a []kubeapi.Pod) string { return showing(len(v), len(a)) })
	selected := ggui.Bind(ggui.Map(m.Pod, kubeapi.Pod.Key).Get, m.SelectPod)
	toolbar := widget.Row(
		widget.Expanded(filterBar(m.PodQuery, tr("Filter by name, namespace or status"), count)),
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
	).BindSelected(selected).RowName(kubeapi.Pod.Name).RowHeight(48).MaxRows(6).
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
	return ui.MenuOf(glyph(lucide.Ellipsis, 16),
		ui.MenuItem(tr("Logs"), func() { m.ShowPodLogs(p) }),
		ui.MenuItem(tr("Copy name"), func() { m.Copy(p.Name(), p.Name()) }),
		ui.MenuItem(tr("Copy kubectl logs command"), func() { m.Copy(kubectlLogs(link, p, ctr), "kubectl logs") }),
		ui.MenuItem(tr("Copy kubectl exec command"), func() { m.Copy(kubectlExec(link, p, ctr), "kubectl exec") }),
		ui.MenuItem(tr("Copy kubectl describe command"), func() { m.Copy(kubectlDescribe(link, p), "kubectl describe") }),
		ui.MenuDivider(),
		ui.MenuItem(tr("Delete…"), func() { m.AskDeletePod(p) }).Destructive(),
	).Name(tr("More actions for %s", p.Name()))
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
	return detailHead(p.Name(), podStatusLabel(p), meta,
		ui.Button(tr("Delete"), func() { m.AskDeletePod(p) }).Outline().Icon(glyph(lucide.Trash, 14)).Disabled(p.Metadata.Deleted != nil),
		podMenu(m, p),
	)
}

// podLogsView shows the last lines the chosen container of the chosen pod
// wrote, read again as often as the pods are, with a choice of its
// containers when it has more than one.
func podLogsView(m *Model) ggui.Widget {
	names := ggui.Map(m.Pod, kubeapi.Pod.ContainerNames).WithEqual(slices.Equal[[]string])
	picker := ggui.View(names, func(list []string) ggui.Widget {
		if len(list) < 2 {
			return widget.Box()
		}
		p := ggui.Peek(m.Pod)
		pick := ggui.Bind(m.PodLogContainer.Get, m.PodContainer.Set)
		return ui.Select(pick).Options(list).Format(func(n string) string {
			if p.IsInit(n) {
				return tr("%s (init)", n)
			}
			return n
		}).Small().MinWidth(140).Name(tr("Container"))
	})
	return engineLogsView(m, m.PodLogs,
		tr("The last %d lines, as kubectl logs shows them.", podLogTail), tr("The container has written nothing yet."),
		picker,
		tip(iconButton(lucide.RefreshCw, tr("Refresh logs"), m.LoadPodLogs), tr("Refresh")))
}

// podInfo tells what the pod is made of and where it runs, and the
// commands that reach it.
func podInfo(m *Model, p kubeapi.Pod) ggui.Widget {
	parts := []ggui.Widget{sectionTitle(tr("Containers"), "")}
	for _, c := range append(slices.Clone(p.Spec.Containers), p.Spec.InitContainers...) {
		parts = append(parts, podContainerRow(p, c))
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

// podContainerRow is a container of a pod: its state, name, image and
// restarts.
func podContainerRow(p kubeapi.Pod, c kubeapi.Container) ggui.Widget {
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
	).Gap(10).Align(widget.AlignCenter)
}

// podsSummary counts the cluster's pods and leads to the Pods page, which
// lists them.
func podsSummary(m *Model) ggui.Widget {
	line := ggui.Derived(func() [2]string {
		a, l := m.PodsAccess.Get(), m.Pods.Get()
		switch {
		case a != PodsOpen:
			return [2]string{a.Reason(), ""}
		case m.PodsWaiting.Get():
			return [2]string{tr("The cluster is starting…"), tr("Its pods show here once the API server answers. k3s takes up to two minutes to come up.")}
		case l.Err != "":
			return [2]string{tr("Could not reach the API server"), l.Err}
		case !l.Loaded:
			return [2]string{tr("Asking the API server…"), ""}
		}
		running, total, _ := podCounts(l.Items)
		var mine []string
		for _, p := range userPods(l.Items, false) {
			mine = append(mine, p.Name())
		}
		return [2]string{tr("%d running · %d total", running, total), strings.Join(mine, ", ")}
	})
	text := ggui.View(line, func(t [2]string) ggui.Widget {
		col := []ggui.Widget{widget.Text(t[0]).Size(13).Weight(ggui.WeightMedium)}
		if t[1] != "" {
			col = append(col, ui.Caption(t[1]).Size(12).NoWrap().Ellipsis())
		}
		return widget.Column(col...).Gap(2)
	})
	return ui.Card(widget.Row(
		glyph(lucide.Hexagon, 20).Color(colorGray),
		widget.Expanded(text),
		ui.Button(tr("Open Pods"), func() { m.Go(PagePods) }).Outline().Size(ui.ButtonSmall).
			Icon(glyph(lucide.ArrowRight, 14)).BindDisabled(ggui.Not(m.PodsOpen)),
	).Gap(14).Align(widget.AlignCenter))
}
