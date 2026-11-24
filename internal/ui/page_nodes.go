package ui

import (
	"slices"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// nodesCard lists the cluster's nodes on the Kubernetes page, with Add node,
// and Remove for each agent.
func nodesCard(m *Model) ggui.Widget {
	nodes := ggui.Combine(m.Rows, m.KubeNodes, clusterNodes).
		WithEqual(func(a, b []ClusterNode) bool { return slices.EqualFunc(a, b, ClusterNode.Equal) })
	type addState struct {
		why    string
		adding bool
	}
	blocked := ggui.Combine(m.Rows, m.AddingNode, func(rows []Row, adding bool) addState {
		return addState{why: AddNodeBlocked(rows), adding: adding}
	})
	add := ggui.View(blocked, func(st addState) ggui.Widget {
		if st.adding {
			return ui.ButtonOf(widget.Row(ui.Spinner().Size(12), widget.Text(tr("Adding node…"))).Gap(6).Align(widget.AlignCenter), nil).
				Outline().Size(ui.ButtonSmall).Name(tr("Add node")).Disabled(true)
		}
		b := ui.Button(tr("Add node"), m.AddNode).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.Plus, 14)).Disabled(st.why != "")
		if st.why != "" {
			return tip(b, st.why)
		}
		return b
	})
	return ggui.View(nodes, func(ns []ClusterNode) ggui.Widget {
		var list ggui.Widget = ui.Caption(tr("Set up Kubernetes to add nodes."))
		if len(ns) > 0 {
			rows := make([]ggui.Widget, 0, 2*len(ns))
			for i, n := range ns {
				if i > 0 {
					rows = append(rows, ui.Divider())
				}
				rows = append(rows, nodeRow(m, n))
			}
			list = widget.Column(rows...).Gap(8).Stretch()
		}
		return ui.Card(
			ui.CardHeader(trn(len(ns), "%d node", "%d nodes"),
				tr("Each node runs in a sandbox of its own. Agents join the server on a network the app makes between them.")).Action(add),
			ui.CardContent(list),
		)
	})
}

// nodeRow is a node: its sandbox's state, its name and role, its address,
// and what the cluster says of it.
func nodeRow(m *Model, n ClusterNode) ggui.Widget {
	role := tr("agent")
	if n.Server {
		role = tr("server")
	}
	facts := []string{role}
	if n.Addr != "" {
		facts = append(facts, n.Addr)
	}
	switch {
	case !n.Row.State.Status.Active():
	case n.Row.State.Status == sandbox.Stopping:
		facts = append(facts, tr("Stopping"))
	case n.Row.State.Paused:
		facts = append(facts, tr("Paused"))
	case n.Ready:
		facts = append(facts, tr("Ready"))
	case n.Registered:
		facts = append(facts, tr("Not ready"))
	default:
		facts = append(facts, tr("Joining…"))
	}
	if n.Version != "" {
		facts = append(facts, n.Version)
	}
	id := n.Row.Spec.ID
	actions := []ggui.Widget{
		tip(iconButton(lucide.ScrollText, tr("Logs of %s", n.Row.Spec.Name), func() { m.ShowSandbox(id, 0) }), tr("Logs")),
	}
	if !n.Server {
		actions = append(actions, tip(iconButton(lucide.Trash, tr("Remove %s", n.Row.Spec.Name), func() { m.AskRemoveNode(id) }), tr("Remove node")))
	}
	return widget.Row(
		statusMark(n.Row.State),
		widget.Expanded(widget.Column(
			widget.Text(n.Row.Spec.Name).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			ui.Caption(strings.Join(facts, " · ")).Size(12).NoWrap().Ellipsis(),
		).Gap(2)),
		widget.Row(actions...).Gap(2),
	).Gap(10).Align(widget.AlignCenter)
}
