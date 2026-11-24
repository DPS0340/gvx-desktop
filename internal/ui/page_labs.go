package ui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/mcpserver"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// amd64BuilderPlatform reports whether the host supports the Rosetta amd64
// builder VM (Apple silicon macOS). Tests may replace it.
var amd64BuilderPlatform = func() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

// labsPage holds experimental features: local *.localhost HTTPS routing, the
// built-in AI agent MCP server, and (on Apple silicon macOS) amd64 builds in a
// Rosetta VM.
func labsPage(m *Model) ggui.Widget {
	warning := widget.Row(
		glyph(lucide.FlaskConical, 15).Color(colorAmber),
		widget.Expanded(ui.Caption(tr("These features are still being built. They may change or go away in an update, and turning one off undoes what it set up."))),
	).Gap(8).Align(widget.AlignStart)
	sections := []ggui.Widget{
		headed(pageHeader(tr("Labs"), ui.Caption(tr("Experimental features to try before they are done.")))),
		widget.Box(warning).Pad(10, 12).Radius(8).Fill(rgbA(0xf59e0b, 0x14)).Border(1, rgbA(0xf59e0b, 0x55)),
		settingsGroup(lucide.Globe, tr("Local HTTPS routing"),
			localProxySettings(m),
		),
		settingsGroup(lucide.Bot, tr("AI agent MCP server"),
			mcpServerSettings(m),
		),
	}
	if amd64BuilderPlatform() {
		sections = append(sections, settingsGroup(lucide.Cpu, tr("Cross-architecture builds"),
			amd64BuilderSettings(m),
		))
	}
	return page(sections...)
}

func localProxySettings(m *Model) ggui.Widget {
	enabled := ggui.Bind(func() bool { return m.Settings.Get().LocalProxy }, m.SetLocalProxy)

	initSet := ggui.Peek(m.Settings)
	httpPort := ggui.State(strconv.Itoa(initSet.LocalProxyHTTPPortOrDefault()))
	httpsPort := ggui.State(strconv.Itoa(initSet.LocalProxyHTTPSPortOrDefault()))
	ggui.Watch(m.Settings, func(s storeSettings) {
		wantHTTP := strconv.Itoa(s.LocalProxyHTTPPortOrDefault())
		if ggui.Peek(httpPort) != wantHTTP {
			httpPort.Set(wantHTTP)
		}
		wantHTTPS := strconv.Itoa(s.LocalProxyHTTPSPortOrDefault())
		if ggui.Peek(httpsPort) != wantHTTPS {
			httpsPort.Set(wantHTTPS)
		}
	})

	commitPorts := func(string) {
		cur := ggui.Peek(m.Settings)
		h, err1 := strconv.Atoi(strings.TrimSpace(ggui.Peek(httpPort)))
		s, err2 := strconv.Atoi(strings.TrimSpace(ggui.Peek(httpsPort)))
		if err1 != nil || err2 != nil || h < 1 || h > 65535 || s < 1 || s > 65535 || h == s {
			httpPort.Set(strconv.Itoa(cur.LocalProxyHTTPPortOrDefault()))
			httpsPort.Set(strconv.Itoa(cur.LocalProxyHTTPSPortOrDefault()))
			return
		}
		m.SetLocalProxyPorts(h, s)
	}

	toggleRow := settingRow(
		tr("Route *.localhost domains"),
		tr("Serve running containers, Compose services, sandboxes, and Kubernetes port forwards on http://<name>.localhost and https://<name>.localhost without editing /etc/hosts."),
		ui.Switch(enabled, "").Name(tr("Route *.localhost domains")),
	)

	portsRow := settingRow(
		tr("Proxy ports"),
		tr("Loopback TCP ports on 127.0.0.1 for HTTP and HTTPS requests. Changes apply immediately."),
		widget.Row(
			ui.Caption("HTTP").Size(12),
			widget.Box(ui.TextField(httpPort).Filter(digits).Name(tr("HTTP port")).OnCommit(commitPorts)).Width(84),
			ui.Caption("HTTPS").Size(12),
			widget.Box(ui.TextField(httpsPort).Filter(digits).Name(tr("HTTPS port")).OnCommit(commitPorts)).Width(84),
		).Gap(8).Align(widget.AlignCenter),
	)

	details := ggui.View(m.LocalProxy, func(st LocalProxyState) ggui.Widget {
		var rows []ggui.Widget

		// Status row.
		var dotColor color.Color = colorGray
		stateText := tr("Off")
		var statusActions []ggui.Widget
		switch {
		case st.Err != "":
			dotColor = colorRed
			stateText = tr("Could not start local proxy: %s", st.Err)
			statusActions = append(statusActions, ui.Button(tr("Retry"), m.RetryLocalProxy).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RefreshCw, 14)))
		case st.Running:
			dotColor = colorGreen
			stateText = tr("Running on *.localhost (HTTP :%d · HTTPS :%d)", st.HTTPPort, st.HTTPSPort)
		case st.Enabled:
			dotColor = colorAmber
			stateText = tr("Starting…")
		}
		statusParts := []ggui.Widget{
			dot(dotColor, 7),
			widget.Expanded(widget.Text(stateText).Size(13).Weight(ggui.WeightMedium)),
		}
		statusParts = append(statusParts, statusActions...)
		rows = append(rows, widget.Row(statusParts...).Gap(8).Align(widget.AlignCenter))

		// Local Dev CA row.
		if st.CAPath != "" || st.Running {
			caPath := st.CAPath
			var caActions []ggui.Widget
			if caPath != "" {
				caDir := filepath.Dir(caPath)
				trustCmd := localproxy.TrustCommand(caPath)
				caActions = append(caActions,
					ui.Button(tr("Reveal CA"), func() { m.Open(caDir) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.FolderOpen, 13)),
				)
				if trustCmd != "" {
					caActions = append(caActions,
						ui.Button(tr("Copy trust command"), func() { m.Copy(trustCmd, tr("CA trust command")) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Copy, 13)),
					)
				}
			}
			caRow := widget.Row(
				glyph(lucide.ShieldCheck, 14).Color(colorGreen),
				widget.Expanded(widget.Column(
					widget.Text(tr("Local development CA")).Size(12).Weight(ggui.WeightMedium),
					ui.Mono(tildePath(caPath)).Size(11).Color(colorGray).NoWrap().Ellipsis(),
				).Gap(2)),
				widget.Row(caActions...).Gap(6).Align(widget.AlignCenter),
			).Gap(10).Align(widget.AlignCenter)
			rows = append(rows, widget.Box(caRow).Pad(10).Radius(8).Border(1, colorBorder))
		}

		// Active routes preview when running.
		if st.Running {
			if len(st.Routes) == 0 {
				rows = append(rows, ui.Caption(tr("No active routes yet. Start a container, sandbox, or Kubernetes port forward with a published TCP port.")).Size(12))
			} else {
				routeRows := make([]ggui.Widget, 0, len(st.Routes))
				for _, rt := range st.Routes {
					httpsURL := localproxy.FormatURL("https", rt.Host, st.HTTPSPort)
					target := rt.Target
					routeRows = append(routeRows, widget.Row(
						ui.Badge(rt.Kind).Outline(),
						ui.Button(httpsURL, func() { m.Open(httpsURL) }).Ghost().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.ExternalLink, 12)),
						ui.Mono("→ "+target).Size(12).Color(colorGray).NoWrap(),
						widget.Spacer(),
						tip(iconButton(lucide.Copy, tr("Copy %s", httpsURL), func() { m.Copy(httpsURL, httpsURL) }), tr("Copy URL")),
					).Gap(8).Align(widget.AlignCenter))
				}
				rows = append(rows, widget.Column(
					ui.Caption(trn(len(st.Routes), "%d active route", "%d active routes")).Size(12),
					widget.Box(widget.Column(routeRows...).Gap(4).Stretch()).Pad(8, 10).Radius(8).Border(1, colorBorder),
				).Gap(6).Stretch())
			}
		}

		return widget.Column(rows...).Gap(10).Stretch()
	})

	return widget.Column(
		toggleRow,
		ui.Divider(),
		portsRow,
		ui.Divider(),
		details,
	).Gap(12).Stretch()
}

func mcpServerSettings(m *Model) ggui.Widget {
	enabled := ggui.Bind(func() bool { return m.Settings.Get().MCPServer }, m.SetMCPServer)

	port := ggui.State(strconv.Itoa(ggui.Peek(m.Settings).MCPPortOrDefault()))
	ggui.Watch(m.Settings, func(s storeSettings) {
		wantPort := strconv.Itoa(s.MCPPortOrDefault())
		if ggui.Peek(port) != wantPort {
			port.Set(wantPort)
		}
	})
	commitPort := func(v string) {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 || n > 65535 {
			port.Set(strconv.Itoa(ggui.Peek(m.Settings).MCPPortOrDefault()))
			return
		}
		m.SetMCPPort(n)
	}

	toggleRow := settingRow(
		tr("Built-in MCP server"),
		tr("Expose a local Streamable HTTP Model Context Protocol (MCP) endpoint on 127.0.0.1 so AI coding agents can create sandboxes, run commands, take and restore snapshots, and inspect containers, images, and routes."),
		ui.Switch(enabled, "").Name(tr("Built-in MCP server")),
	)

	portRow := settingRow(
		tr("MCP port"),
		tr("Loopback TCP port on 127.0.0.1 for /mcp requests. Default is %d.", store.DefaultMCPPort),
		widget.Box(ui.TextField(port).Filter(digits).Name(tr("MCP port")).OnCommit(commitPort)).Width(84),
	)

	details := ggui.View(m.MCPServer, func(st MCPServerState) ggui.Widget {
		var rows []ggui.Widget

		// Status row.
		var dotColor color.Color = colorGray
		stateText := tr("Off")
		var statusTrailing []ggui.Widget
		switch {
		case st.Err != "":
			dotColor = colorRed
			stateText = tr("Could not start MCP server: %s", st.Err)
			statusTrailing = append(statusTrailing, ui.Button(tr("Retry"), m.RetryMCPServer).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RefreshCw, 14)))
		case st.Running:
			dotColor = colorGreen
			stateText = tr("Running on %s", st.EndpointURL)
			if st.CallCount > 0 {
				activity := trn(int(st.CallCount), "%d call", "%d calls")
				if st.LastTool != "" {
					activity += " · " + st.LastTool
				}
				statusTrailing = append(statusTrailing, ui.Badge(activity).Outline())
			}
		case st.Enabled:
			dotColor = colorAmber
			stateText = tr("Starting…")
		}
		statusParts := []ggui.Widget{
			dot(dotColor, 7),
			widget.Expanded(widget.Text(stateText).Size(13).Weight(ggui.WeightMedium)),
		}
		statusParts = append(statusParts, statusTrailing...)
		rows = append(rows, widget.Row(statusParts...).Gap(8).Align(widget.AlignCenter))

		// Quick copy bar when enabled or running.
		if st.Enabled || st.Running {
			ep := st.EndpointURL
			if ep == "" {
				p := st.Port
				if p <= 0 {
					p = store.DefaultMCPPort
				}
				ep = fmt.Sprintf("http://127.0.0.1:%d/mcp", p)
			}
			claudeCmd := mcpserver.ClaudeAddCommand(ep)
			jsonCfg := mcpserver.ClientConfigJSON(ep)
			rows = append(rows, widget.Wrap(
				ui.Button(tr("Copy endpoint"), func() { m.Copy(ep, ep) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Copy, 13)),
				ui.Button(tr("Copy Claude Code command"), func() { m.Copy(claudeCmd, tr("Claude Code command")) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Terminal, 13)),
				ui.Button(tr("Copy JSON config"), func() { m.Copy(jsonCfg, tr("MCP client JSON config")) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Copy, 13)),
			).Gap(8))
		}

		rows = append(rows, ui.Caption(tr("14 tools: sandboxes, exec, logs, snapshots, containers, images, routes.")).Size(12))

		return widget.Column(rows...).Gap(10).Stretch()
	})

	return widget.Column(
		toggleRow,
		ui.Divider(),
		portRow,
		ui.Divider(),
		details,
	).Gap(12).Stretch()
}
