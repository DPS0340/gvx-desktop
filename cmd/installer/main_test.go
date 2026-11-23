package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/installer"
)

func makeTestPayload(t *testing.T, meta installer.Metadata, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	w, err := zw.Create("meta.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(rawMeta); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		fw, err := zw.Create("files/" + rel)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDescribe(t *testing.T) {
	app := newInstallerApp()
	desc := app.Describe()
	if desc.Name != "GVX Desktop" {
		t.Errorf("Name = %q, want GVX Desktop", desc.Name)
	}
	if desc.ID != "io.github.ironpark.gvx-desktop" {
		t.Errorf("ID = %q", desc.ID)
	}
	if !desc.Windows.Enabled || desc.Windows.Scope != installer.ScopeUser {
		t.Errorf("Windows = %+v", desc.Windows)
	}
	if !desc.Windows.StartMenuShortcut || !desc.Windows.DesktopShortcut || !desc.Windows.LaunchAfter {
		t.Errorf("Windows defaults = %+v", desc.Windows)
	}
	if desc.MacOS.Enabled {
		t.Errorf("MacOS.Enabled = true, want false")
	}
	if desc.Windows.LicenseFile != "LICENSE" {
		t.Errorf("LicenseFile = %q, want LICENSE", desc.Windows.LicenseFile)
	}
}

func TestWizardFreshAndLegacyUpgrade(t *testing.T) {
	oldStop := stopRunningApp
	stopRunningApp = func() {}
	t.Cleanup(func() { stopRunningApp = oldStop })

	payload := makeTestPayload(t, installer.Metadata{
		Name:              "GVX Desktop",
		ID:                "io.github.ironpark.gvx-desktop",
		Version:           "0.9.0",
		Executable:        "GVX Desktop.exe",
		Scope:             installer.ScopeUser,
		License:           "Apache License 2.0",
		StartMenuShortcut: true,
		DesktopShortcut:   true,
		LaunchAfter:       true,
	}, map[string][]byte{
		"GVX Desktop.exe":                []byte("MZnewapp"),
		"ggfx_driver.dll":                []byte("MZnewdll"),
		"bin/docker.exe":                 []byte("MZdocker"),
		"cli-plugins/docker-buildx.exe":  []byte("MZbuildx"),
		"cli-plugins/docker-compose.exe": []byte("MZcompose"),
	})

	t.Run("fresh install starts on license step and reaches options", func(t *testing.T) {
		pb := installer.NewPreviewBackend()
		defer pb.Close()

		app := newInstallerApp()
		app.Payload = payload

		var ic *installer.Context
		var p *ggui.Probe
		p = ggui.ProbeBuilder(func() ggui.Widget {
			return ic.BuildWidget(p.Post, nil, func() {})
		}, ggui.Sz(560, 430))
		p.Setup(func() {
			var err error
			ic, err = installer.NewContext(app, nil, pb)
			if err != nil {
				t.Fatal(err)
			}
		})
		defer p.Close()
		p.Frame()

		if ic.IsUpgrade() {
			t.Fatal("fresh install reported IsUpgrade=true")
		}
		st, ok := ic.CurrentStep()
		if !ok || st.ID != "license" {
			t.Fatalf("first step = %q, want license", st.ID)
		}
		ic.LicenseAccepted.Set(true)
		ic.Next()
		p.Frame()
		if st, _ := ic.CurrentStep(); st.ID != "location" {
			t.Fatalf("second step = %q, want location", st.ID)
		}
		ic.Next()
		p.Frame()
		if st, _ := ic.CurrentStep(); st.ID != "options" {
			t.Fatalf("third step = %q, want options", st.ID)
		}
		if !strings.Contains(p.Semantics().String(), "Add bundled Docker CLI") {
			t.Errorf("options step missing Docker CLI checkbox:\n%s", p.Semantics())
		}
	})

	t.Run("legacy install.ps1 upgrade skips license and cleans old updater files", func(t *testing.T) {
		pb := installer.NewPreviewBackend()
		defer pb.Close()

		installDir := filepath.Join(pb.Root(), "Programs", "GVX Desktop")
		if err := os.MkdirAll(filepath.Join(installDir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Simulate an existing install.ps1 installation (no registry entry, no manifest)
		// with leftover legacy updater files.
		for _, f := range []string{
			filepath.Join(installDir, "GVX Desktop.exe"),
			filepath.Join(installDir, "GVX Desktop.old.exe"),
			filepath.Join(installDir, "ggfx_driver.old.dll"),
			filepath.Join(installDir, ".gvx-desktop-download-12345"),
			filepath.Join(installDir, "bin", "docker.new.exe"),
		} {
			if err := os.WriteFile(f, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		app := newInstallerApp()
		app.Payload = payload

		var ic *installer.Context
		var p *ggui.Probe
		p = ggui.ProbeBuilder(func() ggui.Widget {
			return ic.BuildWidget(p.Post, nil, func() {})
		}, ggui.Sz(560, 430))
		p.Setup(func() {
			var err error
			ic, err = installer.NewContext(app, nil, pb)
			if err != nil {
				t.Fatal(err)
			}
		})
		defer p.Close()
		p.Frame()

		if !ic.IsUpgrade() {
			t.Fatal("legacy install.ps1 installation should be detected as upgrade")
		}
		if st, _ := ic.CurrentStep(); st.ID != "location" {
			t.Fatalf("upgrade should skip license step; first step = %q", st.ID)
		}

		// Advance through options -> progress (runs install).
		ic.Next()
		ic.Next()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if st, _ := ic.CurrentStep(); st.ID == "finish" {
				break
			}
			if ggui.Peek(ic.Failed) {
				t.Fatalf("install failed: %s", ggui.Peek(ic.ErrText))
			}
			time.Sleep(10 * time.Millisecond)
		}
		if st, _ := ic.CurrentStep(); st.ID != "finish" {
			t.Fatalf("expected finish step, got %q", st.ID)
		}

		// Verify all bundled files were installed and legacy swap artifacts were removed.
		for _, rel := range []string{
			"GVX Desktop.exe",
			"ggfx_driver.dll",
			"bin/docker.exe",
			"cli-plugins/docker-buildx.exe",
			"cli-plugins/docker-compose.exe",
			"install-manifest.json",
		} {
			if _, err := os.Stat(filepath.Join(installDir, filepath.FromSlash(rel))); err != nil {
				t.Errorf("missing installed file %s: %v", rel, err)
			}
		}
		for _, gone := range []string{
			filepath.Join(installDir, "GVX Desktop.old.exe"),
			filepath.Join(installDir, "ggfx_driver.old.dll"),
			filepath.Join(installDir, ".gvx-desktop-download-12345"),
			filepath.Join(installDir, "bin", "docker.new.exe"),
		} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("legacy updater artifact %s was not cleaned", gone)
			}
		}
	})

	t.Run("uninstall removes Windows Run autostart registry value", func(t *testing.T) {
		pb := installer.NewPreviewBackend()
		defer pb.Close()

		// Pre-populate autostart Run key as GVX Desktop would.
		if err := pb.SetRegistryValue(installer.RegistryHKCU, runRegistryKey, "io.github.ironpark.gvx-desktop", installer.StringValue(`"C:\GVX Desktop.exe" -hidden`)); err != nil {
			t.Fatal(err)
		}
		installDir := filepath.Join(pb.Root(), "Programs", "GVX Desktop")

		app := newInstallerApp()
		app.Payload = payload

		// First install silently into installDir, then uninstall silently.
		var ic *installer.Context
		var p *ggui.Probe
		p = ggui.ProbeBuilder(func() ggui.Widget {
			return ic.BuildWidget(p.Post, nil, func() {})
		}, ggui.Sz(560, 430))
		p.Setup(func() {
			var err error
			ic, err = installer.NewContext(app, []string{"--uninstall", "--dir", installDir}, pb)
			if err != nil {
				t.Fatal(err)
			}
		})
		defer p.Close()
		p.Frame()

		ic.StartUninstall()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if st, _ := ic.CurrentStep(); st.ID == "finish" {
				break
			}
			if ggui.Peek(ic.Failed) {
				t.Fatalf("uninstall failed: %s", ggui.Peek(ic.ErrText))
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, exists, err := pb.GetRegistryValue(installer.RegistryHKCU, runRegistryKey, "io.github.ironpark.gvx-desktop"); err != nil || exists {
			t.Errorf("Run registry value should be removed on uninstall: exists=%v err=%v", exists, err)
		}
	})

	t.Run("after install adds ~/.gvx/bin to PATH when option checked", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		t.Setenv("USERPROFILE", tmpHome)
		t.Setenv("DOCKER_CONFIG", filepath.Join(tmpHome, ".docker"))

		pb := installer.NewPreviewBackend()
		defer pb.Close()

		// Use non-Windows executable name when running this test on macOS/Linux
		// because dockercli.BundleAt probes for runtime.GOOS executable names.
		dockerRel := "bin/docker"
		buildxRel := "cli-plugins/docker-buildx"
		if filepath.Separator == '\\' {
			dockerRel += ".exe"
			buildxRel += ".exe"
		}
		cliPayload := makeTestPayload(t, installer.Metadata{
			Name:       "GVX Desktop",
			ID:         "io.github.ironpark.gvx-desktop",
			Version:    "0.9.0",
			Executable: "GVX Desktop.exe",
			Scope:      installer.ScopeUser,
		}, map[string][]byte{
			"GVX Desktop.exe": []byte("MZapp"),
			dockerRel:         []byte("#!/bin/sh\n"),
			buildxRel:         []byte("#!/bin/sh\n"),
		})

		app := newInstallerApp()
		app.Payload = cliPayload

		var ic *installer.Context
		var p *ggui.Probe
		p = ggui.ProbeBuilder(func() ggui.Widget {
			return ic.BuildWidget(p.Post, nil, func() {})
		}, ggui.Sz(560, 430))
		p.Setup(func() {
			var err error
			ic, err = installer.NewContext(app, nil, pb)
			if err != nil {
				t.Fatal(err)
			}
		})
		defer p.Close()
		p.Frame()

		ic.Bool(optAddDockerPath, false).Set(true)
		ic.StartInstall()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if st, _ := ic.CurrentStep(); st.ID == "finish" {
				break
			}
			if ggui.Peek(ic.Failed) {
				t.Fatalf("install failed: %s", ggui.Peek(ic.ErrText))
			}
			time.Sleep(10 * time.Millisecond)
		}

		wantBin := filepath.Join(tmpHome, ".gvx", "bin")
		if dirs := pb.PathDirs(installer.ScopeUser); !slices.Contains(dirs, wantBin) {
			t.Errorf("user PATH = %v, want %q", dirs, wantBin)
		}
	})
}
