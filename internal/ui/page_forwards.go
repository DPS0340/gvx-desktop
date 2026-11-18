package ui

import (
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"
)

// forwardsCard lists the port forwards that run, over each view of the
// Pods page, with what can be done to each; nothing while none runs.
func forwardsCard(m *Model) ggui.Widget {
	return ggui.View(m.Forwards, func(list []PortForward) ggui.Widget {
		if len(list) == 0 {
			return widget.Box()
		}
		rows := []ggui.Widget{sectionTitle(tr("Port forwards"),
			tr("Ports of this computer the app carries into the cluster. They stop when you stop them, when their pod or service goes, when the cluster stops, and when the app quits."))}
		for _, f := range list {
			rows = append(rows, forwardRow(m, f))
		}
		return widget.Padding(ui.Card(widget.Column(rows...).Gap(10).Stretch()), 0, 0, 16, 0)
	})
}

// forwardRow is a port forward: where it listens and what it reaches, how
// its connections went, and its actions.
func forwardRow(m *Model, f PortForward) ggui.Widget {
	col, state := colorGreen, trn(f.Served, "%d connection carried", "%d connections carried")
	if f.Err != "" {
		col, state = colorAmber, tr("The last connection failed")
	}
	status := widget.Row(ui.Caption(f.Target.Namespace + " · " + state).Size(12).NoWrap().Ellipsis()).Gap(6)
	var line ggui.Widget = status
	if f.Err != "" {
		line = tip(status, f.Err)
	}
	return widget.Row(
		widget.Box(dot(col, 8)).Pad(2),
		widget.Expanded(widget.Column(
			widget.Row(
				ui.Mono(f.Address()).Size(13),
				glyph(lucide.ArrowRight, 13).Color(colorGray),
				widget.Flexible(widget.Text(f.TargetText()).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
			).Gap(8).Align(widget.AlignCenter),
			line,
		).Gap(2)),
		tip(iconButton(lucide.ExternalLink, tr("Open %s", f.URL()), func() { m.Open(f.URL()) }), tr("Open in browser")),
		tip(iconButton(lucide.Copy, tr("Copy address %s", f.Address()), func() { m.Copy(f.Address(), f.Address()) }), tr("Copy address")),
		ui.Button(tr("Stop"), func() { m.StopForward(f.ID, "") }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.Square, 12)),
	).Gap(8).Align(widget.AlignCenter)
}

// digits keeps the digits of what is typed in a port's field.
func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// forwardDialog starts a port forward to a pod or a service: the port of
// it, offered from those it declares, and the port of this computer.
func forwardDialog(m *Model) ggui.Widget {
	title := ui.HeadingOf(ggui.Map(m.ForwardDraft, func(d ForwardDraft) string { return tr("Forward a port to %s", d.What()) })).Size(17)
	text := ui.CaptionOf(ggui.Map(m.ForwardDraft, func(d ForwardDraft) string {
		if d.Target.Service != "" {
			return tr("Connections to a port of this computer are carried to a ready pod of the service in %s, through the cluster's API server, as kubectl port-forward does.", d.Target.Namespace)
		}
		return tr("Connections to a port of this computer are carried to the pod in %s, through the cluster's API server, as kubectl port-forward does.", d.Target.Namespace)
	}))
	remote := ggui.View(m.ForwardDraft, func(d ForwardDraft) ggui.Widget {
		label, help := tr("Port of the pod"), tr("The pod declares no ports; type the one it listens on.")
		if d.Target.Service != "" {
			label, help = tr("Port of the service"), tr("The service has no TCP ports; type one.")
		}
		field := ui.TextField(m.ForwardRemote).Filter(digits).Placeholder("80").Name(label).OnSubmit(func(string) { m.SubmitForward() })
		f := ui.Field(label, field)
		switch len(d.Ports) {
		case 0:
			return f.Help(help)
		case 1:
			return f // the one it has is in the field
		}
		var chips []ggui.Widget
		for _, p := range d.Ports {
			text := itoa(p.Port)
			if p.Name != "" {
				text += " · " + p.Name
			}
			chips = append(chips, ui.Badge(text).Outline().OnTap(func() { m.PickForwardPort(p.Port) }))
		}
		return widget.Column(f, widget.Wrap(chips...).Gap(6)).Gap(8).Stretch()
	})
	local := ui.Field(tr("Port of this computer"),
		ui.TextField(m.ForwardLocal).Filter(digits).Placeholder("8080").Name(tr("Port of this computer")).OnSubmit(func(string) { m.SubmitForward() })).
		Help(tr("On 127.0.0.1, so only this computer reaches it. The same number is offered when it is free, and past 8000 for one below 1024."))
	failed := ggui.View(m.ForwardErr, func(err string) ggui.Widget {
		if err == "" {
			return widget.Box()
		}
		return ui.Alert(tr("Could not forward the port"), err).Destructive().Icon(icons.Alert)
	})
	invalid := ggui.Combine(m.ForwardRemote, m.ForwardLocal, func(r, l string) bool {
		_, okR := parsePort(r)
		_, okL := parsePort(l)
		return !okR || !okL
	})
	return ui.Dialog(m.ForwardOpen, widget.Column(
		widget.Column(title, text).Gap(4).Stretch(),
		remote,
		local,
		failed,
	).Gap(14).Stretch()).Name(tr("Forward a port")).Width(480).Footer(
		ui.Button(tr("Cancel"), func() { m.ForwardOpen.Set(false) }).Outline(),
		ui.Button(tr("Forward"), m.SubmitForward).Icon(glyph(lucide.ArrowLeftRight, 14)).BindDisabled(invalid),
	)
}
