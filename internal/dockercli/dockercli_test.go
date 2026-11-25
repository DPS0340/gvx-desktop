package dockercli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func withEnv(t *testing.T, exe, home, targetOS string, env map[string]string) {
	t.Helper()
	oldExe, oldHome, oldOS, oldEnv := executablePath, userHomeDir, goos, getenv
	executablePath = func() (string, error) { return exe, nil }
	userHomeDir = func() (string, error) { return home, nil }
	goos = func() string { return targetOS }
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() {
		executablePath, userHomeDir, goos, getenv = oldExe, oldHome, oldOS, oldEnv
	})
}

func writeExec(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBundledDiscoveryAcrossOSLayouts(t *testing.T) {
	t.Run("macOS app bundle Helpers", func(t *testing.T) {
		root := t.TempDir()
		app := filepath.Join(root, "GVX Desktop.app", "Contents")
		exe := filepath.Join(app, "MacOS", "gvx-desktop")
		docker := filepath.Join(app, "Helpers", "docker")
		buildx := filepath.Join(app, "Helpers", "cli-plugins", "docker-buildx")
		compose := filepath.Join(app, "Helpers", "cli-plugins", "docker-compose")
		writeExec(t, exe, "app")
		writeExec(t, docker, "docker")
		writeExec(t, buildx, "buildx")
		writeExec(t, compose, "compose")

		withEnv(t, exe, t.TempDir(), "darwin", nil)
		b := Bundled()
		if !b.Available() || b.Docker != docker || b.Buildx != buildx || b.Compose != compose {
			t.Fatalf("Bundled() = %+v", b)
		}
		if !IsBundledPath(docker) {
			t.Errorf("IsBundledPath(%q) = false, want true", docker)
		}
	})

	t.Run("Windows sibling bin and cli-plugins", func(t *testing.T) {
		root := t.TempDir()
		exe := filepath.Join(root, "GVX Desktop.exe")
		docker := filepath.Join(root, "bin", "docker.exe")
		buildx := filepath.Join(root, "cli-plugins", "docker-buildx.exe")
		compose := filepath.Join(root, "cli-plugins", "docker-compose.exe")
		writeExec(t, exe, "MZ")
		writeExec(t, docker, "MZ")
		writeExec(t, buildx, "MZ")
		writeExec(t, compose, "MZ")

		withEnv(t, exe, t.TempDir(), "windows", nil)
		b := Bundled()
		if !b.Available() || b.Docker != docker || b.Buildx != buildx || b.Compose != compose {
			t.Fatalf("Bundled() = %+v", b)
		}
	})

	t.Run("Linux usr/lib/gvx-desktop", func(t *testing.T) {
		root := t.TempDir()
		exe := filepath.Join(root, "usr", "bin", "gvx-desktop")
		docker := filepath.Join(root, "usr", "lib", "gvx-desktop", "bin", "docker")
		buildx := filepath.Join(root, "usr", "lib", "gvx-desktop", "cli-plugins", "docker-buildx")
		compose := filepath.Join(root, "usr", "lib", "gvx-desktop", "cli-plugins", "docker-compose")
		writeExec(t, exe, "elf")
		writeExec(t, docker, "elf")
		writeExec(t, buildx, "elf")
		writeExec(t, compose, "elf")

		withEnv(t, exe, t.TempDir(), "linux", nil)
		b := Bundled()
		if !b.Available() || b.Docker != docker || b.Buildx != buildx || b.Compose != compose {
			t.Fatalf("Bundled() = %+v", b)
		}
	})
}

func TestSyncAndConfigPreservation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests Unix symlink behavior")
	}
	root := t.TempDir()
	home := t.TempDir()
	dockerCfgDir := filepath.Join(home, ".docker")
	os.MkdirAll(dockerCfgDir, 0o700)
	// Existing config.json with user keys that must be preserved verbatim.
	cfgPath := filepath.Join(dockerCfgDir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"auths":{"ghcr.io":{}},"credsStore":"osxkeychain"}`), 0o600)

	bundleDir := filepath.Join(root, "bundle")
	writeExec(t, filepath.Join(bundleDir, "docker"), "docker")
	writeExec(t, filepath.Join(bundleDir, "cli-plugins", "docker-buildx"), "buildx")
	writeExec(t, filepath.Join(bundleDir, "cli-plugins", "docker-compose"), "compose")

	withEnv(t, filepath.Join(root, "app"), home, "darwin", map[string]string{
		"GVX_DOCKER_CLI_DIR": bundleDir,
	})

	res, err := Sync(true)
	if err != nil {
		t.Fatal(err)
	}
	wantDocker := filepath.Join(home, ".gvx", "bin", "docker")
	if res.DockerPath != wantDocker {
		t.Fatalf("DockerPath = %q, want %q", res.DockerPath, wantDocker)
	}
	if !IsBundledPath(wantDocker) {
		t.Errorf("IsBundledPath(%q) = false, want true", wantDocker)
	}
	for _, plug := range []string{"docker-buildx", "docker-compose"} {
		if _, err := os.Stat(filepath.Join(home, ".gvx", "cli-plugins", plug)); err != nil {
			t.Errorf("missing synced plugin %s: %v", plug, err)
		}
	}

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["credsStore"] != "osxkeychain" {
		t.Errorf("credsStore lost: %v", parsed)
	}
	extra, _ := parsed["cliPluginsExtraDirs"].([]any)
	wantPlugDir := filepath.Join(home, ".gvx", "cli-plugins")
	if len(extra) != 1 || extra[0] != wantPlugDir {
		t.Errorf("cliPluginsExtraDirs = %v, want [%s]", extra, wantPlugDir)
	}
	// Running Sync again must be idempotent (no duplicate entry).
	if _, err := Sync(true); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(cfgPath)
	var parsed2 map[string]any
	json.Unmarshal(b2, &parsed2)
	if extra2, _ := parsed2["cliPluginsExtraDirs"].([]any); len(extra2) != 1 {
		t.Errorf("duplicate cliPluginsExtraDirs after second Sync: %v", extra2)
	}
}

func TestUnixPathAddAndRemove(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell profile test")
	}
	for _, tc := range []struct {
		name, shell, targetOS, wantProfile, wantLine string
	}{
		{"zsh", "/bin/zsh", "darwin", ".zshrc", `export PATH="$PATH:$HOME/.gvx/bin"`},
		{"bash-linux", "/bin/bash", "linux", ".bashrc", `export PATH="$PATH:$HOME/.gvx/bin"`},
		{"fish", "/opt/homebrew/bin/fish", "darwin", filepath.Join(".config", "fish", "conf.d", "gvx.fish"), `fish_add_path -a "$HOME/.gvx/bin"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			withEnv(t, filepath.Join(home, "app"), home, tc.targetOS, map[string]string{
				"SHELL": tc.shell,
				"PATH":  "/usr/bin:/bin",
			})
			st := PathStatus()
			if st.Active() || st.Managed {
				t.Fatalf("initial PathStatus = %+v, want inactive", st)
			}
			if !strings.HasSuffix(st.Profile, tc.wantProfile) {
				t.Fatalf("Profile = %q, want suffix %q", st.Profile, tc.wantProfile)
			}

			st, err := AddToPath()
			if err != nil {
				t.Fatal(err)
			}
			if !st.Active() || !st.InProfile || !st.Managed {
				t.Fatalf("after AddToPath = %+v", st)
			}
			body, err := os.ReadFile(st.Profile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), tc.wantLine) {
				t.Errorf("profile missing %q:\n%s", tc.wantLine, body)
			}

			// Idempotent second call.
			if _, err := AddToPath(); err != nil {
				t.Fatal(err)
			}
			body2, _ := os.ReadFile(st.Profile)
			if string(body2) != string(body) {
				t.Errorf("AddToPath not idempotent:\n%s", body2)
			}

			st, err = RemoveFromPath()
			if err != nil {
				t.Fatal(err)
			}
			if st.Active() || st.Managed {
				t.Fatalf("after RemoveFromPath = %+v, want inactive", st)
			}
		})
	}
}

func TestSyncBinaryToBinDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests Unix symlink behavior")
	}
	home := t.TempDir()
	withEnv(t, filepath.Join(home, "app"), home, "darwin", map[string]string{
		"SHELL": "/bin/zsh",
		"PATH":  "/usr/bin:/bin",
	})

	// Case 1: binary already inside ~/.gvx/bin is a clean no-op.
	managed := filepath.Join(home, ".gvx", "bin", "rungv")
	writeExec(t, managed, "#!/bin/sh\necho managed\n")
	if !BinaryLinkedInBinDir(managed, "rungv") {
		t.Fatalf("BinaryLinkedInBinDir(managed) = false, want true")
	}
	dst, err := SyncBinaryToBinDir(managed, "rungv")
	if err != nil || dst != managed {
		t.Fatalf("SyncBinaryToBinDir(managed) = (%q, %v), want (%q, nil)", dst, err, managed)
	}

	// Case 2: external binary refuses to clobber an unmanaged regular file.
	ext := filepath.Join(t.TempDir(), "rungv")
	writeExec(t, ext, "#!/bin/sh\necho ext\n")
	if BinaryLinkedInBinDir(ext, "rungv") {
		t.Fatalf("BinaryLinkedInBinDir(ext) = true while unmanaged regular file occupies slot")
	}
	if _, err := SyncBinaryToBinDir(ext, "rungv"); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("SyncBinaryToBinDir(ext) over different regular file = %v; want ErrNotManaged", err)
	}

	// Case 3: once slot is free, external binary is symlinked and recognized.
	if err := os.Remove(managed); err != nil {
		t.Fatal(err)
	}
	dst, err = SyncBinaryToBinDir(ext, "rungv")
	if err != nil || dst != managed {
		t.Fatalf("SyncBinaryToBinDir(ext) = (%q, %v), want (%q, nil)", dst, err, managed)
	}
	if !BinaryLinkedInBinDir(ext, "rungv") {
		t.Fatalf("BinaryLinkedInBinDir(ext) = false after symlink")
	}
	target, err := os.Readlink(managed)
	if err != nil || target != ext {
		t.Fatalf("readlink(%q) = (%q, %v), want %q", managed, target, err, ext)
	}

	// Case 4: updating an existing symlink to a newer external binary works.
	ext2 := filepath.Join(t.TempDir(), "rungv")
	writeExec(t, ext2, "#!/bin/sh\necho ext2\n")
	if _, err := SyncBinaryToBinDir(ext2, "rungv"); err != nil {
		t.Fatalf("SyncBinaryToBinDir(ext2) error: %v", err)
	}
	if !BinaryLinkedInBinDir(ext2, "rungv") {
		t.Fatalf("BinaryLinkedInBinDir(ext2) = false after symlink update")
	}
}
