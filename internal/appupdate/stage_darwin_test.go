package appupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// diskImage builds a disk image as the release's is laid out — an app,
// signed ad hoc, and a link to /Applications — of a made-up app with the
// identifier id, in a temporary folder. tamper changes the app after it is
// signed.
func diskImage(t *testing.T, id string, tamper bool) []byte {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	app := filepath.Join(src, "GVX Desktop.app")
	os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "MacOS", "gvx-desktop"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>`+id+`</string>
<key>CFBundleExecutable</key><string>gvx-desktop</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>0.6.0</string>
</dict></plist>
`), 0o644)
	if out, err := exec.Command("/usr/bin/codesign", "--force", "--deep", "-s", "-", app).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v: %s", err, out)
	}
	if tamper {
		os.WriteFile(filepath.Join(app, "Contents", "MacOS", "gvx-desktop"), []byte("#!/bin/sh\necho changed\n"), 0o755)
	}
	os.Symlink("/Applications", filepath.Join(src, "Applications"))
	dmg := filepath.Join(t.TempDir(), "app.dmg")
	if out, err := exec.Command("/usr/bin/hdiutil", "create", "-quiet", "-srcfolder", src, "-volname", "GVX Desktop", "-format", "UDZO", "-ov", dmg).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil create: %v: %s", err, out)
	}
	b, err := os.ReadFile(dmg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// installed makes a made-up installed app in a temporary Applications
// folder: never the real one.
func installed(t *testing.T) (apps, bundle string) {
	apps = filepath.Join(t.TempDir(), "Applications")
	bundle = filepath.Join(apps, "GVX Desktop.app")
	os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o755)
	os.WriteFile(filepath.Join(bundle, "Contents", "version"), []byte("old"), 0o644)
	return apps, bundle
}

func mounted(t *testing.T, under string) bool {
	out, _ := exec.Command("/usr/bin/hdiutil", "info").Output()
	return strings.Contains(string(out), under)
}

// The whole of a bundle's update, with a disk image built here and a
// temporary Applications folder: download, mount, copy, check, unmount,
// and the helper's swap once the app has ended.
func TestBundleUpdateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and mounts disk images")
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("the app is released for Apple silicon only")
	}
	name, _ := AssetName("v0.6.0", "darwin", "arm64", Bundle)
	rel := serveRelease(t, name, diskImage(t, BundleID, false), "")
	apps, bundle := installed(t)

	s, err := stageBundle(context.Background(), rel, bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s.stage) != apps || !strings.HasPrefix(filepath.Base(s.stage), stagePrefix) {
		t.Errorf("staged in %s, not beside the app", s.stage)
	}
	if !exists(filepath.Join(s.fresh, "Contents", "Info.plist")) {
		t.Fatalf("no new app at %s", s.fresh)
	}
	if read(t, filepath.Join(bundle, "Contents", "version")) != "old" {
		t.Error("staging changed the installed app")
	}
	if mounted(t, "gvx-desktop-update-") {
		t.Error("the disk image is still mounted")
	}
	if err := s.Prepare([]string{"-x"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, s.script), "pid="+strconv.Itoa(os.Getpid())) {
		t.Errorf("the helper waits for another process:\n%s", read(t, s.script))
	}

	// The helper as Finish would start it, but for an app that has ended
	// already and an open that only notes what it was given.
	done := exec.Command("true")
	done.Run()
	bin := t.TempDir()
	opened := filepath.Join(bin, "opened")
	open := filepath.Join(bin, "open")
	os.WriteFile(open, []byte("#!/bin/sh\necho \"$@\" > '"+opened+"'\n"), 0o755)
	os.WriteFile(s.script, []byte(helperScript(done.Process.Pid, s.Target, s.fresh, s.stage, open, nil)), 0o700)
	if out, err := exec.Command("/bin/sh", s.script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if exists(filepath.Join(bundle, "Contents", "version")) || !exists(filepath.Join(bundle, "Contents", "Info.plist")) {
		t.Error("the new app is not in place")
	}
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", bundle).CombinedOutput(); err != nil {
		t.Errorf("the app in place does not verify: %v: %s", err, out)
	}
	if entries, _ := os.ReadDir(apps); len(entries) != 1 {
		t.Errorf("%d entries in Applications", len(entries))
	}
	if read(t, opened) != bundle+"\n" {
		t.Errorf("opened %q", read(t, opened))
	}
}

func TestBundleUpdateRefusesABrokenApp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and mounts disk images")
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("the app is released for Apple silicon only")
	}
	name, _ := AssetName("v0.6.0", "darwin", "arm64", Bundle)
	for _, c := range []struct {
		what   string
		id     string
		tamper bool
		want   string
	}{
		{"changed after it was signed", BundleID, true, "signature"},
		{"another app", "com.example.other", false, "com.example.other"},
	} {
		t.Run(c.what, func(t *testing.T) {
			rel := serveRelease(t, name, diskImage(t, c.id, c.tamper), "")
			apps, bundle := installed(t)
			_, err := stageBundle(context.Background(), rel, bundle, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v", err)
			}
			if entries, _ := os.ReadDir(apps); len(entries) != 1 || read(t, filepath.Join(bundle, "Contents", "version")) != "old" {
				t.Errorf("Applications has %d entries", len(entries))
			}
		})
	}
}
