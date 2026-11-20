package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunArgsDockerRecipeShape(t *testing.T) {
	s := Spec{
		Name:    "docker-engine",
		Image:   "docker:dind",
		Command: "--tls=false",
		Env:     []string{"DOCKER_TLS_CERTDIR="},
		Ports:   []string{"unix:~/.gvx/docker.sock:2375", "127.0.0.1:9000-9019:9000-9019"},
		Mounts:  []Mount{{Type: MountVolume, Source: "gvx-docker", Target: "/var/lib/docker"}},
	}
	got := s.RunArgs(Paths{})
	want := []string{"run", "--name", "docker-engine", "-e", "DOCKER_TLS_CERTDIR=",
		"-p", "unix:~/.gvx/docker.sock:2375", "-p", "127.0.0.1:9000-9019:9000-9019",
		"-v", "gvx-docker:/var/lib/docker", "docker:dind", "--tls=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRunArgsEverything(t *testing.T) {
	home, _ := os.UserHomeDir()
	s := Spec{
		Name: "web", Image: "nginx:alpine", Command: `sh -c "nginx -g 'daemon off;'"`,
		Entrypoint: "", SetEntrypoint: true,
		Hostname: "h", User: "1000:1000", Workdir: "/app",
		Memory: "512m", CPUs: "1.5", Backend: "hvf", Unprivileged: true, HostCACerts: true, Pull: "always",
		Network: "nat", IsolateLAN: true,
		Workspace: "~/site", WorkspaceRO: true, RootfsVolume: "root",
		Mounts: []Mount{
			{Type: MountBind, Source: "~/data", Target: "/data", ReadOnly: true},
			{Type: MountVolume, Source: "/disk/pg.ext4", Target: "/pg"},
			{Type: MountTmpfs, Target: "/scratch", Size: "64m"},
		},
		ExtraArgs: "--tune net.nat.mtu=9000",
	}
	got := strings.Join(s.RunArgs(Paths{ControlSocket: "/r/s.sock", CheckpointDir: "/r/ck"}), " ")
	for _, part := range []string{
		"--entrypoint  ", "--user 1000:1000", "--memory 512m", "--cpus 1.5", "--backend hvf",
		"--privileged=false", "--host-ca-certs", "--pull always", "--workdir /app", "-isolate-lan",
		"-workspace " + filepath.Join(home, "site") + " -ro", "-rootfs-volume root",
		"-v " + filepath.Join(home, "data") + ":/data:ro", "--mount type=volume,src=/disk/pg.ext4,dst=/pg",
		"--tmpfs /scratch:size=64m", "-control-socket /r/s.sock", "-checkpoint-dir /r/ck",
		"--tune net.nat.mtu=9000 nginx:alpine sh -c nginx -g 'daemon off;'",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in\n%s", part, got)
		}
	}
	if strings.Contains(got, "-net ") {
		t.Error("nat is the default and needs no flag")
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreArgs(t *testing.T) {
	s := Spec{Name: "a", Image: "alpine", Command: "sleep 9", Env: []string{"A=1"}, Memory: "1g",
		Ports: []string{"8080:80"}, Workspace: "/w", Mounts: []Mount{{Type: MountVolume, Source: "v", Target: "/v"}}}
	got := s.RestoreArgs("/ck", Paths{ControlSocket: "/s"})
	want := []string{"restore", "-checkpoint-dir", "/ck", "-control-socket", "/s", "--name", "a", "-p", "8080:80", "-workspace", "/w", "-v", "v:/v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestValidatePort(t *testing.T) {
	ok := []string{"80", "8080:80", "127.0.0.1:8080:80", "8000-8009:8000-8009", "8000-8009", "53:53/udp", "unix:~/.gvx/d.sock:2375", "[::1]:8080:80"}
	bad := []string{"", "x", "8000-8009:80", "70000:80", "unix:/a,b:1", "unix:nopath", "1-2000"}
	for _, p := range ok {
		if err := ValidatePort(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range bad {
		if ValidatePort(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestValidate(t *testing.T) {
	base := Spec{Name: "a", Image: "alpine"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Spec){
		func(s *Spec) { s.Name = "" },
		func(s *Spec) { s.Name = "has space" },
		func(s *Spec) { s.Image = "" },
		func(s *Spec) { s.Memory = "lots" },
		func(s *Spec) { s.CPUs = "-1" },
		func(s *Spec) { s.Command = `"open` },
		func(s *Spec) { s.Mounts = []Mount{{Type: MountTmpfs, Target: "/proc/x"}} },
		func(s *Spec) { s.Mounts = []Mount{{Type: MountTmpfs, Target: "/a"}, {Type: MountTmpfs, Target: "/a"}} },
		func(s *Spec) { s.Workspace = "/w"; s.Mounts = []Mount{{Type: MountTmpfs, Target: "/workspace"}} },
		func(s *Spec) {
			s.RootfsVolume = "v"
			s.Mounts = []Mount{{Type: MountVolume, Source: "v", Target: "/v"}}
		},
		func(s *Spec) { s.Network = "none"; s.Ports = []string{"80"} },
		func(s *Spec) { s.Env = []string{"=x"} },
	}
	for i, mutate := range bad {
		s := base.Clone()
		mutate(&s)
		if s.Validate() == nil {
			t.Errorf("case %d accepted: %+v", i, s)
		}
	}
}

func TestSuggestName(t *testing.T) {
	for in, want := range map[string]string{
		"nginx:alpine": "nginx", "rancher/k3s:v1.31": "k3s", "ghcr.io/a/b@sha256:x": "b", "": "sandbox",
	} {
		if got := SuggestName(in); got != want {
			t.Errorf("SuggestName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEqualAndClone(t *testing.T) {
	a := Spec{Name: "a", Env: []string{"X=1"}, Mounts: []Mount{{Type: MountTmpfs, Target: "/t"}}}
	b := a.Clone()
	if !a.Equal(b) {
		t.Fatal("clone differs")
	}
	b.Env[0] = "X=2"
	if a.Env[0] != "X=1" || a.Equal(b) {
		t.Fatal("clone shares its slices")
	}
	if !(Spec{}).Equal(Spec{Env: []string{}}) {
		t.Fatal("nil and empty differ")
	}
}

func TestLogBuffer(t *testing.T) {
	b := NewLogBuffer(100)
	for range 250 {
		b.Add(Stdout, "x")
	}
	b.Add(Stderr, "err")
	if n := len(b.Snapshot()); n > 100 || n < 90 {
		t.Errorf("kept %d lines", n)
	}
	if b.Seq() != 251 {
		t.Errorf("seq %d", b.Seq())
	}
	if tail := b.Tail(1, Stderr); len(tail) != 1 || tail[0] != "err" {
		t.Errorf("tail %q", tail)
	}
}

func TestAutoPublish(t *testing.T) {
	s := Spec{ExtraArgs: "--tune a=b"}
	if HasAutoPublish(s) || AutoPublishSet(s) {
		t.Fatal("a spec without the flag has it")
	}
	on := WithAutoPublish(s, true)
	if !HasAutoPublish(on) || on.ExtraArgs != "--tune a=b -auto-publish" {
		t.Fatalf("on: %q", on.ExtraArgs)
	}
	if again := WithAutoPublish(on, true); again.ExtraArgs != on.ExtraArgs {
		t.Fatalf("twice: %q", again.ExtraArgs)
	}
	if off := WithAutoPublish(on, false); HasAutoPublish(off) || off.ExtraArgs != "--tune a=b" {
		t.Fatalf("off: %q", off.ExtraArgs)
	}
	no := Spec{ExtraArgs: "--auto-publish=false"}
	if HasAutoPublish(no) || !AutoPublishSet(no) {
		t.Fatal("-auto-publish=false is set, and off")
	}
	if !HasAutoPublish(Spec{ExtraArgs: "--auto-publish"}) {
		t.Fatal("two dashes")
	}
}
