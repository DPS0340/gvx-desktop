package ui

import (
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
)

// workloadsView is the Workloads view of the cluster: the Deployments,
// StatefulSets, DaemonSets and Jobs that make the cluster's pods, filtered
// via the toolbar query and System workloads switch.
func workloadsView(m *Model) ggui.Widget {
	type viewState struct {
		stage int
		err   string
	}
	state := ggui.Derived(func() viewState {
		l := m.Workloads.Get()
		switch {
		case !l.Loaded:
			return viewState{stage: 0}
		case l.Err != "":
			return viewState{stage: 1, err: l.Err}
		}
		return viewState{stage: 2}
	})
	return ggui.View(state, func(s viewState) ggui.Widget {
		switch s.stage {
		case 0:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Asking the API server…"))).Gap(8)
		case 1:
			return ui.Alert(tr("Could not list the workloads"), s.err).Destructive().Icon(icons.Alert)
		}
		return workloadsSection(m)
	})
}

func workloadsSection(m *Model) ggui.Widget {
	type emptyState struct {
		visible  int
		total    int
		filtered bool
		system   bool
	}
	st := ggui.Derived(func() emptyState {
		total := m.Workloads.Get().Items
		sys := m.SystemPods.Get()
		q := m.WorkloadQuery.Get()
		vis := filterWorkloads(userWorkloads(total, sys), q)
		return emptyState{
			visible:  len(vis),
			total:    len(total),
			filtered: strings.TrimSpace(q) != "",
			system:   sys,
		}
	})
	note := ggui.View(st, func(s emptyState) ggui.Widget {
		if s.visible > 0 {
			return widget.Box()
		}
		if s.filtered {
			return widget.Padding(widget.Row(
				widget.Expanded(ui.Caption(tr("No workloads match the current filter.")).Size(12)),
				ui.Button(tr("Clear filter"), func() { m.WorkloadQuery.Set("") }).Outline().Size(ui.ButtonExtraSmall),
			).Gap(8).Align(widget.AlignCenter), 8, 12)
		}
		if !s.system && s.total > 0 {
			return widget.Padding(
				ui.Caption(trn(s.total, "%d system workload hidden by the switch above.", "%d system workloads hidden by the switch above.")).Size(12),
				10, 12,
			)
		}
		return widget.Padding(ui.Caption(tr("No workloads in the cluster yet.")).Size(12), 10, 12)
	})
	return tableCard(widget.Column(workloadsTable(m), note).Stretch())
}

func workloadsTable(m *Model) ggui.Widget {
	all := ggui.Combine(m.Workloads, m.SystemPods, func(l Listing[kubeapi.Workload], system bool) []kubeapi.Workload {
		return userWorkloads(l.Items, system)
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.WorkloadQuery, filterWorkloads).WithEqual(nil)
	count := ggui.Combine(visible, m.Workloads, func(v []kubeapi.Workload, l Listing[kubeapi.Workload]) string { return showing(len(v), len(l.Items)) })
	selected := ggui.State("")
	toolbar := widget.Row(
		widget.Expanded(filterBar(m.WorkloadQuery, tr("Filter by kind, name, namespace or image"), count)),
		ui.Switch(m.SystemPods, tr("System workloads")).Small(),
	).Gap(16).Align(widget.AlignCenter)
	pods := ggui.Map(m.Pods, func(l Listing[kubeapi.Pod]) []kubeapi.Pod { return l.Items }).WithEqual(nil)
	return ui.Table(visible, kubeapi.Workload.Key,
		ui.Col(tr("Kind"), func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			type state struct {
				kind string
				h    kubeapi.Health
				why  string
			}
			return ggui.View(ggui.Combine(r, pods, func(w kubeapi.Workload, p []kubeapi.Pod) state {
				h, why := workloadHealth(w, p)
				return state{w.Kind, h, why}
			}), func(s state) ggui.Widget {
				return stateLabel(healthColor(s.h), s.kind, s.h == kubeapi.Changing, s.why)
			})
		}).W(150),
		ui.Col(tr("Name"), func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, kubeapi.Workload.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, kubeapi.Workload.Namespace)).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).Grow(3),
		ui.Col(tr("Ready"), func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			return widget.TextOf(ggui.Map(r, kubeapi.Workload.ReadyText)).Size(13).NoWrap()
		}).W(76),
		ui.Col(tr("Up to date"), func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			return widget.TextOf(ggui.Map(r, func(w kubeapi.Workload) string {
				if w.Kind == kubeapi.KindJob {
					return "—"
				}
				return itoa(w.UpToDate)
			})).Size(13).NoWrap()
		}).W(96),
		ui.Col(tr("Age"), func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(w kubeapi.Workload, now time.Time) string {
				if w.Metadata.Created.IsZero() {
					return "—"
				}
				return since(w.Metadata.Created, now)
			})).Size(12).NoWrap()
		}).W(110),
		ui.Col(tr("Images"), func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(w kubeapi.Workload) string { return strings.Join(w.Images, ", ") }), func(s string) ggui.Widget {
				return tip(ui.Mono(s).Size(12).NoWrap().Ellipsis(), s)
			})
		}).Grow(2),
		ui.Col("", func(r ggui.Readable[kubeapi.Workload]) ggui.Widget {
			return ggui.View(r, func(w kubeapi.Workload) ggui.Widget {
				return widget.Row(
					tip(iconButton(lucide.Hexagon, tr("Pods of %s", w.Name()), func() { m.ShowWorkloadPods(w) }), tr("Show pods")),
					workloadMenu(m, w),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(selected).RowName(kubeapi.Workload.Name).RowHeight(48).Toolbar(toolbar)
}

// workloadMenu is what can be done to a workload.
func workloadMenu(m *Model, w kubeapi.Workload) ggui.Widget {
	link := ggui.Peek(m.KubeLink.View)
	deleting := w.Metadata.Deleted != nil
	items := []ggui.Widget{
		ui.MenuItem(tr("Show pods"), func() { m.ShowWorkloadPods(w) }),
	}
	if w.Scalable() {
		items = append(items, ui.MenuItem(tr("Scale…"), func() { m.AskScale(w) }).Disabled(deleting))
	}
	if w.Restartable() {
		items = append(items, ui.MenuItem(tr("Restart…"), func() { m.AskRestart(w) }).Disabled(deleting))
	}
	items = append(items,
		ui.MenuDivider(),
		ui.MenuItem(tr("Copy name"), func() { m.Copy(w.Name(), w.Name()) }),
		ui.MenuItem(tr("Copy kubectl describe command"), func() { m.Copy(kubectlWorkload(link, w, "describe"), "kubectl describe") }),
	)
	if w.Scalable() {
		cmd := kubectlWorkload(link, w, "scale", "--replicas="+itoa(w.Desired))
		items = append(items, ui.MenuItem(tr("Copy kubectl scale command"), func() { m.Copy(cmd, "kubectl scale") }))
	}
	if w.Restartable() {
		cmd := kubectlCommand(link, "rollout", "restart", strings.ToLower(w.Kind)+"/"+w.Name(), "-n", w.Namespace())
		items = append(items, ui.MenuItem(tr("Copy kubectl rollout restart command"), func() { m.Copy(cmd, "kubectl rollout restart") }))
	}
	items = append(items,
		ui.MenuDivider(),
		ui.MenuItem(tr("Delete…"), func() { m.AskDeleteWorkload(w) }).Destructive().Disabled(deleting),
	)
	return moreMenu(tr("More actions for %s", w.Name()), items...)
}

// scaleDialog sets how many pods a Deployment or StatefulSet runs.
func scaleDialog(m *Model) ggui.Widget {
	title := ui.HeadingOf(ggui.Map(m.ScaleTarget, func(w kubeapi.Workload) string { return tr("Scale %s", w.Name()) })).Size(17)
	text := ui.CaptionOf(ggui.Map(m.ScaleTarget, func(w kubeapi.Workload) string {
		return tr("%s %s · wanted now: %d · ready: %d", w.Kind, w.Name(), w.Desired, w.Ready)
	}))
	scale := func() {
		w, n := ggui.Peek(m.ScaleTarget), int(ggui.Peek(m.ScaleReplicas))
		m.ScaleOpen.Set(false)
		if n == w.Desired {
			return
		}
		m.ScaleWorkload(w, n)
	}
	note := ggui.View(ggui.Combine(m.ScaleTarget, m.ScaleReplicas, func(w kubeapi.Workload, n float64) string {
		switch {
		case n == 0:
			return tr("At 0, all its pods stop; it stays, to be scaled up again.")
		case w.Kind == kubeapi.KindStatefulSet:
			return tr("A StatefulSet adds and removes its pods one at a time, the last first.")
		}
		return ""
	}), func(s string) ggui.Widget {
		if s == "" {
			return widget.Box()
		}
		return widget.Padding(ui.Caption(s).Size(12), 14, 0, 0, 0)
	})
	return ui.Dialog(m.ScaleOpen, widget.Column(
		widget.Padding(widget.Column(title, text).Gap(4), 0, 32, 0, 0),
		widget.Column(
			ui.Field(tr("Replicas"), ui.NumberInput(m.ScaleReplicas).Range(0, 100).Step(1).Precision(0).Name(tr("Replicas"))),
			note,
		).Stretch(),
	).Gap(14).Stretch()).
		Name(tr("Scale")).Width(420).
		Footer(
			ui.Button(tr("Cancel"), func() { m.ScaleOpen.Set(false) }).Outline(),
			ui.Button(tr("Scale"), scale),
		)
}
