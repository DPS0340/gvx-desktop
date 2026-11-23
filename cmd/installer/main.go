// Command installer builds the self-contained Windows setup and uninstaller
// wizard (GVX Desktop-Setup.exe) using github.com/ironpark/ggui/installer.
//
// `ggui build --target windows --installer` discovers ./cmd/installer
// automatically, reads its build-time specification via GGUI_INSTALLER_DESCRIBE,
// embeds GVX Desktop.exe, ggfx_driver.dll, and the bundled Docker CLI binaries
// (bin/docker.exe, cli-plugins/docker-buildx.exe, cli-plugins/docker-compose.exe),
// and produces GVX Desktop-Setup.exe.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/installer"
	"github.com/ironpark/ggui/ui"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/dockercli"
)

const (
	optAddDockerPath = "add_docker_path"
	runRegistryKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
)

// stopRunningApp closes any running GVX Desktop process before installing or
// uninstalling. Tests override it so unit tests never touch host processes.
var stopRunningApp = stopRunningAppPlatform

func main() {
	installer.Main(newInstallerApp())
}

func newInstallerApp() *installer.App {
	preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
	return &installer.App{
		Name:        "GVX Desktop",
		ID:          appupdate.BundleID,
		Description: "Run and manage Linux containers as unprivileged gvx sandboxes, with no VM and no daemon.",
		Publisher:   "ironpark",
		Homepage:    "https://github.com/ironpark/gvx-desktop",
		LicenseFile: "LICENSE",
		Width:       560,
		Height:      430,
		Theme:       preset.Light(),
		DarkTheme:   preset.Dark(),
		Windows: installer.WindowsConfig{
			Scope:             installer.ScopeUser,
			StartMenuShortcut: installer.Bool(true),
			DesktopShortcut:   installer.Bool(true),
			LaunchAfter:       installer.Bool(true),
		},
		// macOS ships as a signed/notarized .dmg and Homebrew Cask.
		MacOS: installer.MacOSConfig{
			Disabled: true,
		},
		Steps: []installer.Step{
			installer.LicenseStep().SkipWhen(func(ctx *installer.Context) bool {
				return ctx.IsUpgrade() || strings.TrimSpace(ctx.Meta().License) == ""
			}),
			installer.LocationStep(),
			optionsStep(),
			installer.ProgressStep(),
			installer.FinishStep(),
		},
		BeforeInstall: func(ctx *installer.Context) error {
			if ctx.IsPreview() {
				return nil
			}
			ctx.ReportProgress(0.04, "Stopping running GVX Desktop…")
			stopRunningApp()
			cleanLegacyUpdaterArtifacts(ctx.Dir())
			return nil
		},
		AfterInstall: func(ctx *installer.Context) error {
			if ctx.IsPreview() || !ctx.GetBool(optAddDockerPath) {
				return nil
			}
			ctx.ReportProgress(0.95, "Configuring bundled Docker CLI PATH…")
			if _, err := dockercli.SyncFromDir(ctx.Dir(), true); err != nil {
				// Non-fatal: GVX Desktop also syncs on launch.
				return nil
			}
			if binDir, err := dockercli.BinDir(); err == nil && binDir != "" {
				return ctx.AddToUserPath(binDir)
			}
			return nil
		},
		BeforeUninstall: func(ctx *installer.Context) error {
			if ctx.IsPreview() {
				return nil
			}
			ctx.ReportProgress(0.05, "Stopping running GVX Desktop…")
			stopRunningApp()
			// Remove login autostart registration created by GVX Desktop settings.
			_ = ctx.DeleteRegistryValue(installer.RegistryHKCU, runRegistryKey, appupdate.BundleID)
			return nil
		},
	}
}

func optionsStep() installer.Step {
	return installer.CustomStep("options", "Options", func(ctx *installer.Context) ggui.Widget {
		addDocker := ctx.Bool(optAddDockerPath, false)
		return widget.Column(
			ui.Checkbox(addDocker, "Add bundled Docker CLI (~/.gvx/bin) to user PATH"),
			ui.Caption("Exposes the bundled docker, docker buildx, and docker compose commands in new terminal windows. You can also change this anytime in Settings → Docker."),
			ui.Divider(),
			ui.Caption("GVX sandboxes use the Windows Hypervisor Platform (WHP). If it is not enabled yet, GVX Desktop will guide you after launch."),
		).Space(1).Align(widget.AlignStretch)
	})
}

// cleanLegacyUpdaterArtifacts removes temporary swap/download files left in
// dir by GVX Desktop <= v0.8.0's legacy in-place self-updater when upgrading
// an existing install.ps1 installation to a Setup-managed installation.
func cleanLegacyUpdaterArtifacts(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	subdirs := []string{
		dir,
		filepath.Join(dir, "bin"),
		filepath.Join(dir, "cli-plugins"),
	}
	patterns := []string{
		"*.old.exe",
		"*.new.exe",
		"*.old.dll",
		"*.new.dll",
		".gvx-desktop-download-*",
	}
	for _, d := range subdirs {
		for _, pat := range patterns {
			matches, err := filepath.Glob(filepath.Join(d, pat))
			if err != nil {
				continue
			}
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
	}
}
