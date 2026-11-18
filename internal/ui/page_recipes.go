package ui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// recipeStatus is the state of a preset's sandbox, or ok false when it
// has not been set up.
func recipeRow(m *Model, key string) ggui.Readable[Row] {
	return ggui.Map(m.Rows, func(rows []Row) Row {
		r, _ := m.RecipeSandbox(rows, key)
		return r
	}).WithEqual(Row.Equal)
}

// recipeHero is the card at the top of a recipe page: what it is, its
// state and its main action.
func recipeHero(m *Model, key string, row ggui.Readable[Row], title, what string, icon *icons.Glyph) ggui.Widget {
	return ggui.View(row, func(r Row) ggui.Widget {
		var actions []ggui.Widget
		var state ggui.Widget
		switch {
		case r.Spec.ID == "":
			state = ui.Caption(tr("Not set up yet."))
			actions = append(actions, ui.Button(tr("Set up and start"), func() { m.CreateFromRecipe(key) }).Icon(glyph(lucide.Play, 14)))
		case r.State.Status.Active():
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button(tr("Logs"), func() { m.ShowSandbox(r.Spec.ID, 0) }).Outline().Icon(glyph(lucide.ScrollText, 14)),
				ui.Button(tr("Stop"), func() { m.StopSandbox(r.Spec.ID) }).Outline().Icon(glyph(lucide.Square, 14)).Disabled(r.State.Status == sandbox.Stopping),
			)
		default:
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button(tr("Settings"), func() { m.EditSandbox(r.Spec.ID) }).Outline(),
				ui.Button(tr("Start"), func() { m.StartSandbox(r.Spec.ID) }).Icon(glyph(lucide.Play, 14)),
			)
		}
		uptimeText := widget.Box()
		if r.State.Status.Active() {
			started := r.State.StartedAt
			uptimeText = widget.Box(ui.CaptionOf(ggui.Map(m.Now, func(now time.Time) string { return tr("up %s", uptime(started, now)) })).Size(12))
		}
		return ui.Card(widget.Row(
			widget.Box(glyph(icon, 26).Color(colorBlue)).Pad(12).Radius(12).Fill(rgbA(0x3b82f6, 0x22)),
			widget.Expanded(widget.Column(
				widget.Row(ui.Heading(title).Size(18), state, uptimeText).Gap(12).Align(widget.AlignCenter),
				ui.Caption(what),
			).Gap(6)),
			widget.Row(actions...).Gap(8),
		).Gap(16).Align(widget.AlignCenter))
	})
}

// ---- Docker -------------------------------------------------------------

func dockerPage(m *Model) ggui.Widget {
	row := recipeRow(m, recipes.Docker)
	// The model follows the engine's containers; see followDocker.
	running := ggui.Map(row, func(r Row) bool { return r.State.Status == sandbox.Running })

	version := ui.CaptionOf(ggui.Combine(running, m.DockerVersion, func(run bool, v dockerapi.Version) string {
		if !run || v.Version == "" {
			return tr("Containers run inside the sandbox; the docker CLI on this machine talks to dockerd over a Unix socket.")
		}
		return fmt.Sprintf("Docker Engine %s (API %s) · %s/%s · kernel %s", v.Version, v.APIVersion, v.OS, v.Arch, v.KernelVersion)
	}))

	return page(
		pageHeader("Docker Engine", version,
			tip(iconButton(lucide.RefreshCw, tr("Refresh containers"), func() { m.LoadDocker(); m.CheckLink(m.DockerLink) }), tr("Refresh")),
		),
		recipeHero(m, recipes.Docker, row, "docker:dind",
			tr("An unmodified dockerd in one sandbox. Images and containers persist in the gvx-docker volume; the API is published on %s.", m.DockerHost()),
			lucide.Container),
		widget.Column(
			sectionTitle(tr("Containers"), tr("Published container ports must be inside 9000–9019, the range the sandbox opened at start.")),
			containersSummary(m),
		).Gap(10).Stretch(),
		widget.Responsive(900,
			widget.Row(widget.Flex(dockerCLICard(m), 1), widget.Flex(dockerNotesCard(), 1)).Gap(16).Align(widget.AlignStart),
			widget.Column(dockerCLICard(m), dockerNotesCard()).Gap(16).Stretch(),
		),
	)
}

// containersSummary counts the engine's containers and leads to the
// Containers page, which lists them.
func containersSummary(m *Model) ggui.Widget {
	line := ggui.Combine(m.ContainersAccess, m.DockerContainers, func(a ContainersAccess, l Listing[dockerapi.Container]) [2]string {
		switch {
		case a != ContainersOpen:
			return [2]string{a.Reason(), ""}
		case l.Err != "":
			return [2]string{tr("Could not reach dockerd"), l.Err}
		case !l.Loaded:
			return [2]string{tr("Asking dockerd…"), ""}
		}
		var running []string
		for _, c := range l.Items {
			if c.State == "running" {
				running = append(running, c.Name())
			}
		}
		return [2]string{tr("%d running · %d total", len(running), len(l.Items)), strings.Join(running, ", ")}
	})
	text := ggui.View(line, func(t [2]string) ggui.Widget {
		col := []ggui.Widget{widget.Text(t[0]).Size(13).Weight(ggui.WeightMedium)}
		if t[1] != "" {
			col = append(col, ui.Caption(t[1]).Size(12).NoWrap().Ellipsis())
		}
		return widget.Column(col...).Gap(2)
	})
	return ui.Card(widget.Row(
		glyph(lucide.Boxes, 20).Color(colorGray),
		widget.Expanded(text),
		ui.Button(tr("Open Containers"), func() { m.Go(PageContainers) }).Outline().Size(ui.ButtonSmall).
			Icon(glyph(lucide.ArrowRight, 14)).BindDisabled(ggui.Not(m.ContainersOpen)),
	).Gap(14).Align(widget.AlignCenter))
}

func dockerCLICard(m *Model) ggui.Widget {
	return ui.Card(
		ui.CardHeader(tr("Use it from a terminal"), tr("docker on this machine reaches the engine through a context named gvx.")),
		ui.CardContent(linkPanel(m, m.DockerLink, func(v LinkView) []ggui.Widget {
			return dockerCommands(m, v, copyable(m, "docker run --rm -p 9000:80 nginx:alpine", tr("example")))
		})),
	)
}

// dockerCommands are the commands that set the gvx context up by hand, with
// extra after the one that selects it.
func dockerCommands(m *Model, v LinkView, extra ...ggui.Widget) []ggui.Widget {
	verb := "create"
	if v.C.Exists {
		verb = "update"
	}
	out := []ggui.Widget{
		copyable(m, "docker context "+verb+" gvx --docker host="+v.Want, "docker context "+verb),
		copyable(m, "docker context use gvx", "docker context use"),
	}
	out = append(out, extra...)
	return append(out, copyable(m, "docker context use "+v.Back, tr("switch back")))
}

// linkPanel tells whether a command-line tool on this machine reaches a
// recipe's engine, and offers the one action that would make it so; the
// commands that do the same by hand are folded away under it.
func linkPanel(m *Model, l *Link, commands func(LinkView) []ggui.Widget) ggui.Widget {
	status := ggui.View(l.View, func(v LinkView) ggui.Widget {
		mark, title, detail := linkWords(l, v)
		text := []ggui.Widget{widget.Text(title).Size(13).Weight(ggui.WeightMedium)}
		if detail != "" {
			text = append(text, ui.Caption(detail).Size(12))
		}
		if v.C.Env != "" && v.Status > ContextFailed {
			text = append(text, widget.Row(glyph(lucide.Info, 12).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("%s is set in the environment the app started from. Where it is set, %s follows it, not this context.", v.C.Env, l.Tool)).Size(12)),
			).Gap(6).Align(widget.AlignStart))
		}
		if b := linkButton(m, l, v); b != nil {
			text = append(text, widget.Padding(widget.Row(b), 4, 0, 0, 0))
		}
		return widget.Row(
			widget.Padding(mark, 1, 0, 0, 0),
			widget.Expanded(widget.Column(text...).Gap(3).Stretch()),
			tip(iconButton(lucide.RefreshCw, tr("Check %s again", l.Tool), func() { m.CheckLink(l) }), tr("Check again")),
		).Gap(10).Align(widget.AlignStart)
	})
	cmds := ggui.View(l.View, func(v LinkView) ggui.Widget {
		return widget.Padding(widget.Column(commands(v)...).Gap(8).Stretch(), 4, 0, 0, 0)
	})
	return widget.Column(
		widget.Box(status).Pad(12).Radius(8).Border(1, colorGrayA),
		ui.Collapsible(m.LinkCommandsOpen, tr("Commands to type instead"), cmds),
	).Gap(8).Stretch()
}

// linkWords are a context's mark, its state in a few words, and what
// those leave to say.
func linkWords(l *Link, v LinkView) (ggui.Widget, string, string) {
	mark := func(g *icons.Glyph, c color.Color) ggui.Widget { return glyph(g, 16).Color(c) }
	docker := l.Tool == "docker"
	switch v.Status {
	case ContextUnknown:
		return ui.Spinner().Size(14), tr("Checking %s…", l.Tool), ""
	case ContextNoCLI:
		if docker {
			return mark(lucide.CircleX, colorGray), tr("The docker command was not found."),
				tr("Install the Docker CLI alone (no local engine is needed), or name it in Settings → Docker.")
		}
		return mark(lucide.CircleX, colorGray), tr("The kubectl command was not found."), tr("Install kubectl, then check again.")
	case ContextPending:
		return mark(lucide.Clock, colorAmber), tr("The cluster has not written its kubeconfig yet."),
			tr("The kubeconfig appears at %s once the server is up.", recipes.Kubeconfig())
	case ContextFailed:
		return mark(lucide.TriangleExclamationPoint, colorAmber), tr("%s did not answer", l.Tool), v.C.Err
	case ContextMissing:
		if docker {
			return mark(lucide.CircleDashed, colorGray), tr("No %s context yet.", l.Name), tr("Create it to point docker at %s.", v.Want)
		}
		return mark(lucide.CircleDashed, colorGray), tr("No %s context yet.", l.Name),
			tr("kubectl adds it to %s; the file as it was is kept as %s.", tildePath(UserKubeconfig()), filepath.Base(KubeconfigBackup()))
	case ContextStale:
		if docker {
			return mark(lucide.TriangleExclamationPoint, colorAmber), tr("The %s context is out of date.", l.Name),
				tr("It points at %s; the engine is at %s.", v.C.Host, v.Want)
		}
		return mark(lucide.TriangleExclamationPoint, colorAmber), tr("The %s context is out of date.", l.Name),
			tr("It does not match the cluster's kubeconfig: the cluster was made anew, or its certificates changed.")
	case ContextElsewhere:
		if v.C.Current == "" {
			return mark(lucide.CircleDot, colorBlue), tr("The %s context exists; %s uses no context.", l.Name, l.Tool), ""
		}
		return mark(lucide.CircleDot, colorBlue), tr("The %s context exists; %s is using %s.", l.Name, l.Tool, v.C.Current), ""
	}
	if docker {
		return mark(lucide.CircleCheck, colorGreen), tr("docker uses the gvx context."), tr("docker in a terminal reaches this engine.")
	}
	return mark(lucide.CircleCheck, colorGreen), tr("kubectl uses the gvx-k3s context."), tr("kubectl in a terminal reaches this cluster.")
}

// linkButton is the one action that fits a context's state, or nil.
func linkButton(m *Model, l *Link, v LinkView) *ui.ButtonWidget {
	var b *ui.ButtonWidget
	switch v.Status {
	case ContextNoCLI:
		if l.Tool != "docker" {
			return nil
		}
		b = ui.Button(tr("Open Settings"), func() { m.Go(PageSettings) }).Outline()
	case ContextMissing:
		b = ui.Button(tr("Create & use context"), func() { m.SetUpLink(l) })
	case ContextStale:
		b = ui.Button(tr("Update the context"), func() { m.UpdateLink(l) })
	case ContextElsewhere:
		b = ui.Button(tr("Use the %s context", l.Name), func() { m.UseLink(l) })
	case ContextInUse:
		label := tr("Switch back to %s", v.Back)
		if v.Back == "" {
			label = tr("Stop using %s", l.Name)
		}
		b = ui.Button(label, func() { m.SwitchLinkBack(l) }).Outline()
	default:
		return nil
	}
	return b.Size(ui.ButtonSmall).Disabled(v.C.Busy)
}

func dockerNotesCard() ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return widget.Column(widget.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	return ui.Card(
		ui.CardHeader(tr("Good to know"), tr("Limits that come from running dockerd in a user-space kernel.")),
		ui.CardContent(widget.Column(
			note(tr("Paths in docker run -v"), tr("A -v path names a path in the sandbox, not on the host. Add a bind mount to the sandbox first (Edit → Storage), then use its guest path.")),
			note(tr("Ports"), tr("Host ports are fixed when the sandbox starts. Add more to the range in the sandbox settings and restart.")),
			note(tr("Architecture"), tr("The engine runs images for this CPU (linux/%s) natively.", goarch())),
			note(tr("Limits"), tr("docker run -m and --cpus are enforced; BLOCK I/O reads 0B because the guest has no io controller.")),
		).Gap(10).Stretch()),
	)
}

// ---- Kubernetes ---------------------------------------------------------

func kubernetesPage(m *Model) ggui.Widget {
	row := recipeRow(m, recipes.Kubernetes)
	ggui.Effect(func() ggui.Cleanup {
		if !row.Get().State.Status.Active() {
			return nil
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					m.post(m.CheckKubeconfig)
				}
			}
		}()
		return func() { close(stop) }
	})
	cfg := recipes.Kubeconfig()
	kubeconfig := ggui.View(m.KubeconfigReady, func(ok bool) ggui.Widget {
		if !ok {
			return widget.Row(glyph(lucide.Clock, 14).Color(colorAmber), ui.Caption(tr("The kubeconfig appears at %s once the server is up.", cfg))).Gap(8)
		}
		return widget.Row(glyph(lucide.CircleCheck, 14).Color(colorGreen), ui.Caption(tr("kubeconfig written to ")+cfg),
			widget.Spacer(), ui.Button(tr("Reveal"), func() { m.Open(cfg) }).Outline().Size(ui.ButtonExtraSmall)).Gap(8).Align(widget.AlignCenter)
	})
	return page(
		pageHeader("Kubernetes", ui.Caption(tr("An unmodified k3s node with its kubelet, containerd and flannel, running in one unprivileged sandbox."))),
		recipeHero(m, recipes.Kubernetes, row, tr("k3s single-node cluster"),
			tr("API server on 127.0.0.1:6443, NodePorts 30000–30019 on the host, cluster state in the k3s volume. Starting takes up to two minutes."),
			lucide.ShipWheel),
		widget.Column(
			sectionTitle(tr("Pods"), tr("Pods run on the k3s node in the Kubernetes sandbox.")),
			podsSummary(m),
		).Gap(10).Stretch(),
		widget.Responsive(900,
			widget.Row(widget.Flex(kubectlCard(m, kubeconfig), 1), widget.Flex(kubeNotesCard(), 1)).Gap(16).Align(widget.AlignStart),
			widget.Column(kubectlCard(m, kubeconfig), kubeNotesCard()).Gap(16).Stretch(),
		),
	)
}

func kubectlCard(m *Model, status ggui.Widget) ggui.Widget {
	cfg := recipes.Kubeconfig()
	return ui.Card(
		ui.CardHeader(tr("Use it with kubectl"), tr("kubectl on this machine reaches the cluster through a context named gvx-k3s.")),
		ui.CardContent(linkPanel(m, m.KubeLink, func(LinkView) []ggui.Widget {
			return []ggui.Widget{
				status,
				copyable(m, "export KUBECONFIG="+cfg, "KUBECONFIG"),
				copyable(m, "kubectl get nodes", "kubectl get nodes"),
				copyable(m, "kubectl create deployment web --image=nginx:alpine", tr("example deployment")),
				copyable(m, "kubectl expose deployment web --type=NodePort --port=80 --overrides='{\"spec\":{\"ports\":[{\"port\":80,\"nodePort\":30000}]}}'", tr("example service")),
				ui.Caption(tr("Then open http://127.0.0.1:30000.")),
			}
		})),
	)
}

func kubeNotesCard() ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return widget.Column(widget.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	return ui.Card(
		ui.CardHeader(tr("Good to know"), tr("What the guest kernel supports.")),
		ui.CardContent(widget.Column(
			note("iptables", tr("The node uses iptables-legacy (IPTABLES_MODE=legacy); nftables, IPVS and eBPF are not available, so kube-proxy runs in iptables mode.")),
			note("CNI", tr("flannel and Calico (VXLAN) work, with NetworkPolicy. Cilium needs eBPF and does not run.")),
			note("NodePorts", tr("Only the published range 30000–30019 reaches the host. Widen it in the sandbox settings.")),
			note(tr("Memory limits"), tr("Pod memory limits are enforced by polling, so a short burst can pass them.")),
		).Gap(10).Stretch()),
	)
}

// ---- Settings -----------------------------------------------------------
