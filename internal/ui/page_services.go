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
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// servicesView is the Services view of the Pods page: the services of the
// cluster, the system's only when asked for, with the NodePorts that reach
// this computer and a way to forward a port to any of them.
func servicesView(m *Model) ggui.Widget {
	// 0: not read yet, 1: failed, 2: only the system's, hidden, 3: some.
	state := ggui.Derived(func() int {
		l := m.Services.Get()
		switch {
		case !l.Loaded:
			return 0
		case l.Err != "":
			return 1
		case len(userServices(l.Items, m.SystemPods.Get())) == 0:
			return 2
		}
		return 3
	})
	return ggui.View(state, func(s int) ggui.Widget {
		switch s {
		case 0:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Asking the API server…"))).Gap(8)
		case 1:
			return ui.Alert(tr("Could not list the services"), ggui.Peek(m.Services).Err).Destructive().Icon(icons.Alert)
		case 2:
			return ui.Empty(tr("No services of your own"), tr("Expose a Deployment with kubectl: kubectl expose deployment web --port=80")).
				Media(glyph(lucide.Network, 32).Color(colorGray)).
				Action(ui.Button(tr("Show system services"), func() { m.SystemPods.Set(true) }).Outline()).Outline()
		}
		return tableCard(servicesTable(m))
	})
}

// clusterSpecOf follows the spec of the cluster's sandbox.
func clusterSpecOf(m *Model) ggui.Readable[sandbox.Spec] {
	return ggui.Map(m.Rows, func(rows []Row) sandbox.Spec {
		r, _ := m.RecipeSandbox(rows, recipes.Kubernetes)
		return r.Spec
	}).WithEqual(sandbox.Spec.Equal)
}

func servicesTable(m *Model) ggui.Widget {
	all := ggui.Combine(m.Services, m.SystemPods, func(l Listing[kubeapi.Service], system bool) []kubeapi.Service {
		return userServices(l.Items, system)
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.ServiceQuery, filterServices).WithEqual(nil)
	count := ggui.Combine(visible, all, func(v, a []kubeapi.Service) string { return showing(len(v), len(a)) })
	spec := clusterSpecOf(m)
	toolbar := widget.Row(
		widget.Expanded(filterBar(m.ServiceQuery, tr("Filter by name, namespace, type or selector"), count)),
		ui.Switch(m.SystemPods, tr("System services")).Small(),
	).Gap(16).Align(widget.AlignCenter)
	return ui.Table(visible, kubeapi.Service.Key,
		ui.Col(tr("Name"), func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, kubeapi.Service.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, kubeapi.Service.Namespace)).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).Grow(3),
		ui.Col(tr("Type"), func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, func(s kubeapi.Service) string { return s.Spec.Type })).Size(13).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, func(s kubeapi.Service) string {
					if ip := s.Spec.ClusterIP; ip != "" && ip != "None" {
						return ip
					}
					return tr("headless")
				})).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).W(124),
		ui.Col(tr("Ports"), func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(s kubeapi.Service) string {
				var parts []string
				for _, p := range s.Spec.Ports {
					parts = append(parts, portText(p))
				}
				return strings.Join(parts, ", ")
			}), func(s string) ggui.Widget {
				if s == "" {
					return ui.Caption("—").Size(12)
				}
				return tip(ui.Mono(s).Size(12).NoWrap().Ellipsis(), s)
			})
		}).Grow(2),
		ui.Col(tr("On this computer"), func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			reach := ggui.Combine(r, spec, nodePortReach).WithEqual(nil)
			return ggui.View(reach, func(list []NodePortReach) ggui.Widget { return nodePortCell(m, list) })
		}).W(176),
		ui.Col(tr("Selector"), func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, func(s kubeapi.Service) string {
				if sel := s.SelectorText(); sel != "" {
					return sel
				}
				return "—"
			})).Size(12).NoWrap().Ellipsis()
		}).Grow(2),
		ui.Col(tr("Age"), func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(s kubeapi.Service, now time.Time) string {
				if s.Metadata.Created.IsZero() {
					return "—"
				}
				return since(s.Metadata.Created, now)
			})).Size(12).NoWrap()
		}).W(104),
		ui.Col("", func(r ggui.Readable[kubeapi.Service]) ggui.Widget {
			return ggui.View(r, func(s kubeapi.Service) ggui.Widget {
				why := forwardable(s)
				forward := iconButton(lucide.ArrowLeftRight, tr("Forward a port to %s", s.Name()), func() { m.AskForwardService(s) }).Disabled(why != "")
				if why == "" {
					why = tr("Forward a port…")
				}
				return widget.Row(tip(forward, why), serviceMenu(m, s, ggui.Peek(spec))).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).RowName(kubeapi.Service.Name).RowHeight(48).MaxRows(8).Toolbar(toolbar)
}

// nodePortCell tells of the ports a service takes on the node: the first,
// as a link that opens it when this computer reaches it, or why it does
// not, and how many more there are.
func nodePortCell(m *Model, list []NodePortReach) ggui.Widget {
	if len(list) == 0 {
		return ui.Caption("—").Size(12)
	}
	r := list[0]
	var first ggui.Widget
	if r.Address != "" {
		url := r.URL()
		first = tip(ui.Button(r.Address, func() { m.Open(url) }).Link().Size(ui.ButtonExtraSmall).Pad(0).
			TrailingIcon(glyph(lucide.ExternalLink, 12)).Name(tr("Open %s", url)), tr("Open %s", url))
	} else {
		first = tip(widget.Row(
			glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber),
			ui.Caption(tr("%d · not reachable", r.Port)).Size(12).NoWrap().Ellipsis(),
		).Gap(6).Align(widget.AlignCenter), r.Why)
	}
	if len(list) == 1 {
		return first
	}
	return widget.Row(first, ui.Caption("+"+itoa(len(list)-1)).Size(12)).Gap(6).Align(widget.AlignCenter)
}

// serviceMenu is what can be done with a service.
func serviceMenu(m *Model, s kubeapi.Service, spec sandbox.Spec) ggui.Widget {
	link := ggui.Peek(m.KubeLink.View)
	items := []ggui.Widget{
		ui.MenuItem(tr("Forward a port…"), func() { m.AskForwardService(s) }).Disabled(forwardable(s) != ""),
	}
	for _, r := range nodePortReach(s, spec) {
		if r.Address == "" {
			continue
		}
		url, addr := r.URL(), r.Address
		items = append(items,
			ui.MenuItem(tr("Open %s", url), func() { m.Open(url) }),
			ui.MenuItem(tr("Copy address %s", addr), func() { m.Copy(addr, addr) }),
		)
	}
	items = append(items,
		ui.MenuDivider(),
		ui.MenuItem(tr("Copy name"), func() { m.Copy(s.Name(), s.Name()) }),
		ui.MenuItem(tr("Copy kubectl describe command"), func() {
			m.Copy(kubectlCommand(link, "describe", "service", "-n", s.Namespace(), s.Name()), "kubectl describe")
		}),
	)
	if len(s.Spec.Ports) > 0 && forwardable(s) == "" {
		p := s.Spec.Ports[0].Port
		f := PortForward{Target: kubeapi.Target{Namespace: s.Namespace(), Service: s.Name(), Port: p}, Local: defaultLocal(p)}
		items = append(items, ui.MenuItem(tr("Copy kubectl port-forward command"), func() { m.Copy(kubectlForward(link, f), "kubectl port-forward") }))
	}
	return moreMenu(tr("More actions for %s", s.Name()), items...)
}
