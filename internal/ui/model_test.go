package ui

import (
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestDraftRoundTrip(t *testing.T) {
	for _, spec := range []sandbox.Spec{
		recipes.DockerEngine(),
		recipes.KubernetesNode(),
		{ID: "x", Name: "full", Image: "alpine", Command: `sh -c "echo hi"`, Env: []string{"A=1", "B"},
			Ports: []string{"8080:80", "unix:~/.gvx/a.sock:3000"}, Workspace: "~/w", WorkspaceRO: true,
			RootfsVolume: "root", Memory: "1g", CPUs: "2", Network: "none", Backend: "systrap",
			Hostname: "h", User: "1000", Workdir: "/w", Unprivileged: true, HostCACerts: true, Pull: "always",
			Checkpoint: true, AutoStart: true, ReadyPattern: "ready", ExtraArgs: "--tune a=b",
			Mounts: []sandbox.Mount{
				{Type: sandbox.MountVolume, Source: "data", Target: "/data", ReadOnly: true},
				{Type: sandbox.MountVolume, Source: "/disk/x.ext4", Target: "/x"},
				{Type: sandbox.MountBind, Source: `C:\src`, Target: "/src"},
				{Type: sandbox.MountTmpfs, Target: "/tmp", Size: "64m", ReadOnly: true},
			}},
	} {
		got, err := DraftFrom(spec).Spec()
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}
		if !got.Equal(spec) {
			t.Errorf("%s round trip\n got  %+v\n want %+v", spec.Name, got, spec)
		}
	}
}

func TestDraftDefaults(t *testing.T) {
	d := DraftFrom(sandbox.Spec{Name: "a", Image: "alpine"})
	if d.Network != "nat" || d.Backend != "auto" || d.Pull != "missing" || !d.Privileged {
		t.Fatalf("defaults %+v", d)
	}
	s, err := d.Spec()
	if err != nil || s.Network != "" || s.Backend != "" || s.Pull != "" || s.Unprivileged {
		t.Fatalf("spec %+v %v", s, err)
	}
}

func TestParseMountRejectsRelativeTarget(t *testing.T) {
	if _, _, _, err := parseMount("data:relative"); err == nil {
		t.Fatal("relative target accepted")
	}
}

func TestWrapCommand(t *testing.T) {
	got := wrapCommand(recipes.DockerEngine().RunArgs(sandbox.Paths{}))
	want := "rungv run \\\n  --name docker-engine \\\n  -e DOCKER_TLS_CERTDIR= \\\n"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "\\\n  docker:dind --tls=false --insecure-registry=host.docker.internal:5001") {
		t.Fatalf("got\n%s", got)
	}
}

func TestUniqueName(t *testing.T) {
	specs := []sandbox.Spec{{Name: "web"}, {Name: "web-2"}}
	if n := uniqueName(specs, "web"); n != "web-3" {
		t.Fatal(n)
	}
	if n := uniqueName(specs, "db"); n != "db" {
		t.Fatal(n)
	}
}

// The channel's newest is what the rungv in use is measured against, so
// that a build of main is offered the last release, and a release main.
func TestUpdateIsMeasuredAgainstItsChannel(t *testing.T) {
	const tag, head = "v0.17.10002026110900", "e410633f24cf34fddfde473e48dde172593dc5af"
	engine := func(version, commit string) Engine {
		return Engine{Path: "/bin/rungv", Checked: true, Version: rungv.Version{Version: version, Commit: commit}}
	}
	release := Update{Tag: tag}
	source := Update{Source: true, Commit: head, Ahead: 2}
	for _, c := range []struct {
		name             string
		u                Update
		e                Engine
		available, newer bool
	}{
		{"an older release", release, engine("v0.16.10022026110900", "aaaa"), true, true},
		{"the newest release", release, engine(tag, head), false, false},
		{"a build of main past the release", release, engine("v0.17.10002026110901-0.20261118000000-0123456789ab", "bbbb"), true, false},
		{"main ahead", source, engine(tag, "aaaa"), true, true},
		{"at main's head", Update{Source: true, Commit: head}, engine(tag, head), false, false},
		{"no rungv", release, Engine{Checked: true}, false, true},
		{"nothing known", Update{}, engine(tag, head), false, false},
	} {
		if got := c.u.Available(c.e); got != c.available {
			t.Errorf("%s: Available = %v", c.name, got)
		}
		if got := c.u.Newer(c.e); c.e.Path != "" && c.u.Known() && got != c.newer {
			t.Errorf("%s: Newer = %v", c.name, got)
		}
	}
}
