package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"

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
			state = ui.Caption("Not set up yet.")
			actions = append(actions, ui.Button("Set up and start", func() { m.CreateFromRecipe(key) }).Icon(glyph(lucide.Play, 14)))
		case r.State.Status.Active():
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button("Logs", func() { m.ShowSandbox(r.Spec.ID, 0) }).Outline().Icon(glyph(lucide.ScrollText, 14)),
				ui.Button("Stop", func() { m.StopSandbox(r.Spec.ID) }).Outline().Icon(glyph(lucide.Square, 14)).Disabled(r.State.Status == sandbox.Stopping),
			)
		default:
			state = statusLabel(r.State)
			actions = append(actions,
				ui.Button("Settings", func() { m.EditSandbox(r.Spec.ID) }).Outline(),
				ui.Button("Start", func() { m.StartSandbox(r.Spec.ID) }).Icon(glyph(lucide.Play, 14)),
			)
		}
		uptimeText := ggui.Box()
		if r.State.Status.Active() {
			started := r.State.StartedAt
			uptimeText = ggui.Box(ui.CaptionOf(ggui.Map(m.Now, func(now time.Time) string { return "up " + uptime(started, now) })).Size(12))
		}
		return ui.Card(ggui.Row(
			ggui.Box(glyph(icon, 26).Color(colorBlue)).Pad(12).Radius(12).Fill(rgbA(0x3b82f6, 0x22)),
			ggui.Expanded(ggui.Column(
				ggui.Row(ui.Heading(title).Size(18), state, uptimeText).Gap(12).Align(ggui.AlignCenter),
				ui.Caption(what),
			).Gap(6)),
			ggui.Row(actions...).Gap(8),
		).Gap(16).Align(ggui.AlignCenter))
	})
}

// ---- Docker -------------------------------------------------------------

func dockerPage(m *Model) ggui.Widget {
	row := recipeRow(m, recipes.Docker)
	// Poll the engine while the page shows and the engine runs.
	ggui.Effect(func() ggui.Cleanup {
		if row.Get().State.Status != sandbox.Running {
			return nil
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(3 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					m.post(m.LoadDocker)
				}
			}
		}()
		return func() { close(stop) }
	})
	running := ggui.Map(row, func(r Row) bool { return r.State.Status == sandbox.Running })

	containers := ggui.Map(m.DockerContainers, func(l Listing[dockerapi.Container]) []dockerapi.Container { return l.Items }).WithEqual(nil)
	table := ui.Table(containers, func(c dockerapi.Container) string { return c.ID },
		ui.Col("Container", func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.Column(
				ggui.TextOf(ggui.Map(r, dockerapi.Container.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, func(c dockerapi.Container) string { return c.ID[:min(12, len(c.ID))] })).Size(11),
			).Gap(1)
		}).Grow(2),
		ui.TextCol("Image", func(c dockerapi.Container) string { return c.Image }).Grow(2),
		ui.Col("State", func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, func(c dockerapi.Container) ggui.Widget {
				col := colorGray
				switch c.State {
				case "running":
					col = colorGreen
				case "restarting", "paused":
					col = colorAmber
				case "dead":
					col = colorRed
				}
				return ggui.Column(
					ggui.Row(dot(col, 7), ggui.Text(c.State).Size(12)).Gap(6),
					ui.Caption(c.Status).Size(11).NoWrap().Ellipsis(),
				).Gap(2)
			})
		}).W(170),
		ui.Col("Ports", func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, func(c dockerapi.Container) ggui.Widget {
				var links []ggui.Widget
				for _, p := range c.Ports {
					if p.PublicPort == 0 || p.Type != "tcp" {
						continue
					}
					url := fmt.Sprintf("http://127.0.0.1:%d", p.PublicPort)
					links = append(links, ui.Badge(fmt.Sprintf("%d→%d", p.PublicPort, p.PrivatePort)).Outline().OnTap(func() { m.Open(url) }))
				}
				if len(links) == 0 {
					return ui.Caption(orDash(c.PortsText())).Size(12)
				}
				return ggui.Wrap(links...).Gap(4)
			})
		}).Grow(2),
		ui.Col("", func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, func(c dockerapi.Container) ggui.Widget {
				var main ggui.Widget
				if c.State == "running" {
					main = tip(iconButton(lucide.Square, "Stop "+c.Name(), func() { m.DockerAction("stop", c) }), "Stop")
				} else {
					main = tip(iconButton(lucide.Play, "Start "+c.Name(), func() { m.DockerAction("start", c) }), "Start")
				}
				return ggui.Row(main,
					ui.MenuOf(glyph(lucide.Ellipsis, 16),
						ui.MenuItem("Restart", func() { m.DockerAction("restart", c) }),
						ui.MenuItem("Copy ID", func() { m.Copy(c.ID, c.ID[:min(12, len(c.ID))]) }),
						ui.MenuItem("Copy docker logs command", func() { m.Copy("docker logs -f "+c.Name(), "docker logs") }),
						ui.MenuDivider(),
						ui.MenuItem("Remove", func() {
							m.Confirm("Remove "+c.Name()+"?", "The container is stopped and removed from the engine.", "Remove container", func() { m.DockerAction("remove", c) })
						}).Destructive(),
					).Name("More actions for "+c.Name()),
				).Gap(2).Justify(ggui.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).RowName(dockerapi.Container.Name).RowHeight(52)

	containersCard := ggui.View(ggui.Combine(running, m.DockerContainers, func(run bool, l Listing[dockerapi.Container]) int {
		switch {
		case !run:
			return 0
		case l.Err != "":
			return 1
		case !l.Loaded:
			return 2
		case len(l.Items) == 0:
			return 3
		}
		return 4
	}), func(state int) ggui.Widget {
		switch state {
		case 0:
			return ui.Empty("The engine is not running", "Containers show here while the Docker Engine sandbox runs.").Media(glyph(lucide.Container, 28).Color(colorGray)).Outline()
		case 1:
			return ui.Alert("Could not reach dockerd", ggui.Peek(m.DockerContainers).Err).Destructive().Icon(icons.Alert)
		case 2:
			return ggui.Row(ui.Spinner(), ui.Caption("Asking dockerd…")).Gap(8)
		case 3:
			return ui.Empty("No containers", "Run one with the docker CLI: docker run -d -p 9000:80 nginx:alpine").Media(glyph(lucide.Container, 28).Color(colorGray)).Outline()
		}
		return ui.Card(table).Pad(0)
	})

	version := ui.CaptionOf(ggui.Combine(running, m.DockerVersion, func(run bool, v dockerapi.Version) string {
		if !run || v.Version == "" {
			return "Containers run inside the sandbox; the docker CLI on this machine talks to dockerd over a Unix socket."
		}
		return fmt.Sprintf("Docker Engine %s (API %s) · %s/%s · kernel %s", v.Version, v.APIVersion, v.OS, v.Arch, v.KernelVersion)
	}))

	return page(
		pageHeader("Docker Engine", version,
			tip(iconButton(lucide.RefreshCw, "Refresh containers", m.LoadDocker), "Refresh"),
		),
		recipeHero(m, recipes.Docker, row, "docker:dind",
			"An unmodified dockerd in one sandbox. Images and containers persist in the gvx-docker volume; the API is published on "+recipes.DockerHost()+".",
			lucide.Container),
		ggui.Column(
			sectionTitle("Containers", "Published container ports must be inside 9000–9019, the range the sandbox opened at start."),
			containersCard,
		).Gap(10).Stretch(),
		ggui.Responsive(900,
			ggui.Row(ggui.Flex(dockerCLICard(m), 1), ggui.Flex(dockerNotesCard(), 1)).Gap(16).Align(ggui.AlignStart),
			ggui.Column(dockerCLICard(m), dockerNotesCard()).Gap(16).Stretch(),
		),
	)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func dockerCLICard(m *Model) ggui.Widget {
	host := recipes.DockerHost()
	_, hasCLI := DockerCLI()
	var cliNote ggui.Widget = ggui.Box()
	if !hasCLI {
		cliNote = ui.Caption("The docker command was not found on PATH. Install the Docker CLI alone; no local engine is needed.")
	}
	return ui.Card(
		ui.CardHeader("Use it from a terminal", "One-time setup of a docker context named gvx."),
		ui.CardContent(ggui.Column(
			copyable(m, "docker context create gvx --docker host="+host, "docker context create"),
			copyable(m, "docker context use gvx", "docker context use"),
			copyable(m, "docker run --rm -p 9000:80 nginx:alpine", "example"),
			copyable(m, "docker context use default", "switch back"),
			cliNote,
		).Gap(8).Stretch()),
		ui.CardFooter(ui.Button("Create & use context", m.CreateDockerContext).Size(ui.ButtonSmall).Disabled(!hasCLI)),
	)
}

func dockerNotesCard() ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return ggui.Column(ggui.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	return ui.Card(
		ui.CardHeader("Good to know", "Limits that come from running dockerd in a user-space kernel."),
		ui.CardContent(ggui.Column(
			note("Paths in docker run -v", "A -v path names a path in the sandbox, not on the host. Add a bind mount to the sandbox first (Edit → Storage), then use its guest path."),
			note("Ports", "Host ports are fixed when the sandbox starts. Add more to the range in the sandbox settings and restart."),
			note("Architecture", "The engine runs images for this CPU (linux/"+goarch()+") natively."),
			note("Limits", "docker run -m and --cpus are enforced; BLOCK I/O reads 0B because the guest has no io controller."),
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
			return ggui.Row(glyph(lucide.Clock, 14).Color(colorAmber), ui.Caption("The kubeconfig appears at "+cfg+" once the server is up.")).Gap(8)
		}
		return ggui.Row(glyph(lucide.CircleCheck, 14).Color(colorGreen), ui.Caption("kubeconfig written to "+cfg),
			ggui.Spacer(), ui.Button("Reveal", func() { m.Open(cfg) }).Outline().Size(ui.ButtonExtraSmall)).Gap(8).Align(ggui.AlignCenter)
	})
	return page(
		pageHeader("Kubernetes", ui.Caption("An unmodified k3s node with its kubelet, containerd and flannel, running in one unprivileged sandbox.")),
		recipeHero(m, recipes.Kubernetes, row, "k3s single-node cluster",
			"API server on 127.0.0.1:6443, NodePorts 30000–30019 on the host, cluster state in the k3s volume. Starting takes up to two minutes.",
			lucide.Ship),
		ggui.Responsive(900,
			ggui.Row(ggui.Flex(kubectlCard(m, kubeconfig), 1), ggui.Flex(kubeNotesCard(), 1)).Gap(16).Align(ggui.AlignStart),
			ggui.Column(kubectlCard(m, kubeconfig), kubeNotesCard()).Gap(16).Stretch(),
		),
	)
}

func kubectlCard(m *Model, status ggui.Widget) ggui.Widget {
	cfg := recipes.Kubeconfig()
	return ui.Card(
		ui.CardHeader("Use it with kubectl", "Point kubectl on this machine at the cluster."),
		ui.CardContent(ggui.Column(
			status,
			copyable(m, "export KUBECONFIG="+cfg, "KUBECONFIG"),
			copyable(m, "kubectl get nodes", "kubectl get nodes"),
			copyable(m, "kubectl create deployment web --image=nginx:alpine", "example deployment"),
			copyable(m, "kubectl expose deployment web --type=NodePort --port=80 --overrides='{\"spec\":{\"ports\":[{\"port\":80,\"nodePort\":30080}]}}'", "example service"),
			ui.Caption("Then open http://127.0.0.1:30080."),
		).Gap(8).Stretch()),
	)
}

func kubeNotesCard() ggui.Widget {
	note := func(title, text string) ggui.Widget {
		return ggui.Column(ggui.Text(title).Size(13).Weight(ggui.WeightMedium), ui.Caption(text)).Gap(2)
	}
	return ui.Card(
		ui.CardHeader("Good to know", "What the guest kernel supports."),
		ui.CardContent(ggui.Column(
			note("iptables", "The node uses iptables-legacy (IPTABLES_MODE=legacy); nftables, IPVS and eBPF are not available, so kube-proxy runs in iptables mode."),
			note("CNI", "flannel and Calico (VXLAN) work, with NetworkPolicy. Cilium needs eBPF and does not run."),
			note("NodePorts", "Only the published range 30000–30019 reaches the host. Widen it in the sandbox settings."),
			note("Memory limits", "Pod memory limits are enforced by polling, so a short burst can pass them."),
		).Gap(10).Stretch()),
	)
}

// ---- Settings -----------------------------------------------------------

func settingsPage(m *Model) ggui.Widget {
	set := ggui.Peek(m.Settings)
	path := ggui.State(set.RungvPath)
	cache := ggui.State(set.CacheDir)
	theme := m.Settings.Lens(func(s storeSettings) string { return s.Theme }, func(s storeSettings, v string) storeSettings { s.Theme = v; return s })
	persist := func() { m.UpdateSettings(func(*storeSettings) {}) }
	stopOnQuit := ggui.Bind(func() bool { return m.Settings.Get().StopOnQuit }, func(v bool) { m.UpdateSettings(func(s *storeSettings) { s.StopOnQuit = v }) })
	hideOnClose := ggui.Bind(func() bool { return m.Settings.Get().HideOnClose }, func(v bool) { m.UpdateSettings(func(s *storeSettings) { s.HideOnClose = v }) })
	timeout := ggui.Bind(func() float64 { return float64(m.Settings.Get().StopTimeout) }, func(v float64) {
		m.UpdateSettings(func(s *storeSettings) { s.StopTimeout = max(1, int(v)) })
	})

	engine := ggui.View(m.Engine, func(e Engine) ggui.Widget {
		var rows []ggui.Widget
		if e.Checked && e.Path != "" {
			rows = append(rows,
				kvText("Binary", e.Path),
				kvText("Version", strings.TrimSpace(e.Version.Version+" "+e.Version.Commit)),
				kvText("Built with", e.Version.Go),
				kvText("Backends", strings.Join(e.Info.AvailableBackends, ", ")+" (default "+e.Info.DefaultBackend+")"),
				kvText("Host lockdown", e.Info.HostLockdown),
				kvText("Layer cache", e.Info.LayerCacheDir),
				kvText("Volumes", e.Info.VolumeDir),
			)
		} else if e.Err != "" {
			rows = append(rows, ui.Alert("Engine unavailable", e.Err).Destructive().Icon(icons.Alert))
		}
		return ggui.Column(rows...).Gap(8).Stretch()
	})
	return page(
		pageHeader("Settings", ui.Caption("Where rungv lives and how the app treats running sandboxes.")),
		engineAlert(m),
		ui.Card(
			ui.CardHeader("Engine", "gvx Desktop drives the rungv binary; it never loads gvx into its own process.").
				Action(iconButton(lucide.RefreshCw, "Check again", func() { m.DetectEngine(nil) })),
			ui.CardContent(ggui.Column(
				ui.Field("rungv path", ggui.Row(
					ggui.Expanded(ui.TextField(path).Placeholder("Find automatically (PATH, ~/.gvx/bin, next to the app)").Name("rungv path").
						OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = strings.TrimSpace(v) }) })),
					ui.Button("Browse…", func() {
						browseFile(m, path, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = v }) })
					}).Outline(),
				).Gap(8)).Help("Leave empty to search $GVX_RUNGV, PATH, ~/.gvx/bin and the app's own folder."),
				ui.Field("Layer cache directory", ggui.Row(
					ggui.Expanded(ui.TextField(cache).Placeholder("rungv's default").Name("Layer cache directory").
						OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = strings.TrimSpace(v) }) })),
					ui.Button("Browse…", func() {
						browseFolder(m, cache, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = v }) })
					}).Outline(),
				).Gap(8)).Help("Passed as -cache-dir. Named volumes live in a volumes folder beside it."),
				engine,
				ggui.View(m.Engine, func(e Engine) ggui.Widget {
					if e.Info.OS != "darwin" || e.Path == "" {
						return ggui.Box()
					}
					return ggui.Row(
						ui.Button("Sign rungv", m.SignRungv).Outline().Size(ui.ButtonSmall),
						ui.Caption("Ad-hoc signs rungv with the com.apple.security.hypervisor entitlement for the hvf engine."),
					).Gap(10).Align(ggui.AlignCenter)
				}),
			).Gap(16).Stretch()),
		),
		ui.Card(
			ui.CardHeader("Behavior", "gvx has no daemon: sandboxes run as long as this app does."),
			ui.CardContent(ggui.Column(
				ui.Field("Appearance", ui.ToggleGroup[string](ggui.Bind(theme.Get, func(v string) { theme.Set(v); persist() })).
					Options([]string{"system", "light", "dark"}).Format(strings.Title).Outline().Name("Appearance")),
				ui.Switch(hideOnClose, "Keep running in the menu bar when the window closes"),
				ui.Switch(stopOnQuit, "Stop running sandboxes on quit without asking"),
				ui.Field("Stop timeout (seconds)", ggui.Box(ui.NumberInput(timeout).Name("Stop timeout")).Width(160)).
					Help("How long a sandbox gets to exit after an interrupt before it is killed."),
			).Gap(16).Stretch()),
		),
		ui.Card(
			ui.CardHeader("About", "gvx runs unmodified Linux containers on gVisor's user-space kernel, ported to macOS, Windows and Linux."),
			ui.CardContent(ggui.Row(
				ui.Button("gvx on GitHub", func() { m.Open("https://github.com/gfire-sigs/gvx") }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.ExternalLink, 14)),
				ui.Button("Releases", func() { m.Open("https://github.com/gfire-sigs/gvx/releases") }).Outline().Size(ui.ButtonSmall),
				ui.Button("App data folder", func() { m.Open(m.store.Dir) }).Ghost().Size(ui.ButtonSmall),
			).Gap(8)),
		),
	)
}
