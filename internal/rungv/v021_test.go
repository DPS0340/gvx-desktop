package rungv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFeaturesV021(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { ForgetFeatures(bin) })

	v21 := `#!/bin/sh
cat <<'EOF'
Usage: rungv run [flags] IMAGE
  -allow-bind-attach
  -auto-publish-addr string
  -control-socket string
  -control-token-file string
  -dynamic-ports
  -dynamic-ports-addr value
  -keep-blobs
  -nat-policy-file string
  -net-rx-bps uint
  -net-tx-bps uint
  -unix-forward value
EOF
exit 2
`
	if err := os.WriteFile(bin, []byte(v21), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{Path: bin}
	f := c.Features(context.Background())
	want := Features{
		Control:          true,
		DynamicPorts:     true,
		KeepBlobs:        true,
		UnixForward:      true,
		BindAttach:       true,
		DynamicPortAddrs: true,
		AutoPublishAddr:  true,
		NetQoS:           true,
		NATPolicy:        true,
	}
	if f != want {
		t.Fatalf("v0.21 Features =\n got  %+v\n want %+v", f, want)
	}
}

func TestCacheFlagCommandsPushAndMount(t *testing.T) {
	c := &Client{CacheDir: "/tmp/cache"}
	if got := c.Args("push", "alpine:3.20"); !reflect.DeepEqual(got, []string{"push", "-cache-dir", "/tmp/cache", "alpine:3.20"}) {
		t.Errorf("push args = %q", got)
	}
	// mount ls/rm must NOT receive -cache-dir via Args; only AddMount for volume passes it.
	if got := c.Args("mount", "ls", "-S", "/s"); !reflect.DeepEqual(got, []string{"mount", "ls", "-S", "/s"}) {
		t.Errorf("mount ls args = %q", got)
	}
	if got := c.Args("net-policy", "get", "-S", "/s"); !reflect.DeepEqual(got, []string{"net-policy", "get", "-S", "/s"}) {
		t.Errorf("net-policy get args = %q", got)
	}
}

func TestPushImageAndKeepBlobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	stdinDump := filepath.Join(dir, "stdin.dat")
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { ForgetFeatures(bin) })

	script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -keep-blobs"
  exit 2
fi
: > "` + argsLog + `"
for a in "$@"; do printf '%s\n' "$a" >> "` + argsLog + `"; done
case "$1" in
  push)
    case "$*" in
      *--auth-stdin*) cat > "` + stdinDump + `" ;;
    esac
    case "$*" in
      *fail-push*)
        echo "pushing..."
        echo "rungv push: unauthorized" >&2
        exit 1
        ;;
    esac
    echo "The push refers to repository [ghcr.io/org/app]"
    echo "sha256:abc123: Pushed"
    echo "stderr-note" >&2
    exit 0
    ;;
  pull|load)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{Path: bin, CacheDir: "/tmp/c"}
	ctx := context.Background()

	// Pull / PullPlatform / LoadImagesArch automatically pass -keep-blobs when supported.
	if err := c.Pull(ctx, "alpine:3.20", func(string) {}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	wantPull := []string{"pull", "-cache-dir", "/tmp/c", "-keep-blobs", "alpine:3.20"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantPull) {
		t.Errorf("Pull args = %q, want %q", got, wantPull)
	}

	if err := c.PullPlatform(ctx, "alpine:3.20", "linux/arm64", nil); err != nil {
		t.Fatalf("PullPlatform: %v", err)
	}
	wantPP := []string{"pull", "-cache-dir", "/tmp/c", "-arch", "arm64", "-platform", "linux/arm64", "-keep-blobs", "--", "alpine:3.20"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantPP) {
		t.Errorf("PullPlatform args = %q, want %q", got, wantPP)
	}

	if err := c.LoadImagesArch(ctx, "amd64", strings.NewReader("tar"), nil); err != nil {
		t.Fatalf("LoadImagesArch: %v", err)
	}
	wantLoad := []string{"load", "-cache-dir", "/tmp/c", "-arch", "amd64", "-keep-blobs"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantLoad) {
		t.Errorf("LoadImagesArch args = %q, want %q", got, wantLoad)
	}

	// PushImage with auth payload, derived arch from platform, target, registry URL, quiet.
	var lines []string
	authJSON := `{"username":"u","password":"p"}`
	err := c.PushImage(ctx, PushOptions{
		Ref:         "app:latest",
		Target:      "ghcr.io/org/app:v1",
		Platform:    "linux/amd64",
		RegistryURL: "https://ghcr.io",
		AuthPayload: authJSON,
		Quiet:       true,
	}, func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatalf("PushImage: %v", err)
	}
	wantPush := []string{
		"push", "-cache-dir", "/tmp/c",
		"-arch", "amd64",
		"-platform", "linux/amd64",
		"-registry-url", "https://ghcr.io",
		"-q",
		"--auth-stdin",
		"--", "app:latest", "ghcr.io/org/app:v1",
	}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantPush) {
		t.Errorf("PushImage args = %q, want %q", got, wantPush)
	}
	gotStdin, err := os.ReadFile(stdinDump)
	if err != nil || string(gotStdin) != authJSON {
		t.Errorf("PushImage stdin = %q err=%v, want %q", gotStdin, err, authJSON)
	}
	if len(lines) < 3 {
		t.Errorf("expected streamed stdout+stderr lines, got %q", lines)
	}

	// PushImage without AuthPayload omits --auth-stdin.
	if err := c.PushImage(ctx, PushOptions{Ref: "app:latest"}, nil); err != nil {
		t.Fatalf("PushImage no-auth: %v", err)
	}
	gotNoAuth := readRecordedArgs(t, argsLog)
	if slices.Contains(gotNoAuth, "--auth-stdin") {
		t.Errorf("unexpected --auth-stdin in %q", gotNoAuth)
	}

	// PushImage failure returns *Error.
	err = c.PushImage(ctx, PushOptions{Ref: "fail-push:1"}, nil)
	var re *Error
	if !errors.As(err, &re) || re.Code != 1 || !strings.Contains(re.Stderr, "unauthorized") {
		t.Fatalf("PushImage error = %T %v, want *Error(code=1, unauthorized)", err, err)
	}

	// Validation rejects leading dashes, empty ref, and control chars.
	badOpts := []PushOptions{
		{},
		{Ref: "-bad"},
		{Ref: "ok\nref"},
		{Ref: "ok", Target: "-bad"},
		{Ref: "ok", Target: "bad\x00ref"},
		{Ref: "ok", Arch: "-arm64"},
		{Ref: "ok", Platform: "-linux/amd64"},
		{Ref: "ok", Platform: "linux/\tamd64"},
		{Ref: "ok", RegistryURL: "-https://r"},
	}
	for i, o := range badOpts {
		if err := c.PushImage(ctx, o, nil); err == nil {
			t.Errorf("badOpts[%d] %+v accepted", i, o)
		}
	}
}

func TestV021ControlCommandsWithStub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	bin := filepath.Join(dir, "rungv")

	script := `#!/bin/sh
: > "` + argsLog + `"
for a in "$@"; do printf '%s\n' "$a" >> "` + argsLog + `"; done
cmd="$1"
shift
case "$cmd" in
  port)
    sub="$1"
    shift
    case "$sub" in
      add)
        echo '{"proto":"tcp","host_ip":"127.0.0.1","host_port":18080,"guest_ip":"10.0.2.15","guest_port":80,"source":"dynamic"}'
        exit 0
        ;;
      rm)
        exit 0
        ;;
    esac
    ;;
  mount)
    sub="$1"
    shift
    case "$sub" in
      ls)
        echo '{"target":"/data","type":"hostfs","source":"/Users/me/data","read_only":false,"mutable":true}'
        echo '{"target":"/scratch","type":"tmpfs","source":"tmpfs","read_only":true,"mutable":false}'
        exit 0
        ;;
      add|rm)
        exit 0
        ;;
    esac
    ;;
  update)
    echo '{"memory_bytes":536870912,"cpu_millis":1000,"net_tx_bps":1000000,"net_rx_bps":2000000}'
    exit 0
    ;;
  net-policy)
    sub="$1"
    shift
    case "$sub" in
      get)
        printf '{"default":"deny"}'
        exit 0
        ;;
      replace)
        printf '{"default":"allow"}'
        exit 0
        ;;
    esac
    ;;
esac
echo "unexpected: $cmd $*" >&2
exit 2
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &Client{Path: bin, CacheDir: "/tmp/cache"}
	target := Target{Socket: "/tmp/s.sock", TokenFile: "/tmp/s.tok"}
	ctx := context.Background()

	// AddPorts
	rules, err := c.AddPorts(ctx, target, []string{"18080:80"}, true)
	if err != nil || len(rules) != 1 || rules[0].HostPort != 18080 || rules[0].Source != "dynamic" {
		t.Fatalf("AddPorts: %+v %v", rules, err)
	}
	wantAddPort := []string{"port", "add", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--replace", "-f", "json", "--", "18080:80"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantAddPort) {
		t.Errorf("AddPorts args = %q, want %q", got, wantAddPort)
	}
	if _, err := c.AddPorts(ctx, target, []string{"-bad"}, false); err == nil {
		t.Error("AddPorts accepted leading dash")
	}

	// RemovePorts
	if err := c.RemovePorts(ctx, target, []string{"18080:80"}, true); err != nil {
		t.Fatalf("RemovePorts: %v", err)
	}
	wantRmPort := []string{"port", "rm", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--force", "--", "18080:80"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantRmPort) {
		t.Errorf("RemovePorts args = %q, want %q", got, wantRmPort)
	}

	// Mounts
	mounts, err := c.Mounts(ctx, target)
	if err != nil || len(mounts) != 2 || mounts[0].Target != "/data" || !mounts[0].Mutable || !mounts[1].ReadOnly {
		t.Fatalf("Mounts: %+v %v", mounts, err)
	}
	wantMountLs := []string{"mount", "ls", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "-f", "json"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantMountLs) {
		t.Errorf("Mounts args = %q, want %q", got, wantMountLs)
	}

	// AddMount (bind: no -cache-dir)
	if err := c.AddMount(ctx, target, MountAddOptions{Type: "bind", Source: "/host/dir", Target: "/guest/dir", ReadOnly: true}); err != nil {
		t.Fatalf("AddMount bind: %v", err)
	}
	wantBind := []string{"mount", "add", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--type", "bind", "--src", "/host/dir", "--dst", "/guest/dir", "--ro"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantBind) {
		t.Errorf("AddMount bind args = %q, want %q", got, wantBind)
	}

	// AddMount (tmpfs: --size)
	if err := c.AddMount(ctx, target, MountAddOptions{Type: "tmpfs", Target: "/scratch", Size: "64m"}); err != nil {
		t.Fatalf("AddMount tmpfs: %v", err)
	}
	wantTmpfs := []string{"mount", "add", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--type", "tmpfs", "--dst", "/scratch", "--size", "64m"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantTmpfs) {
		t.Errorf("AddMount tmpfs args = %q, want %q", got, wantTmpfs)
	}

	// AddMount (volume: passes -cache-dir)
	if err := c.AddMount(ctx, target, MountAddOptions{Type: "volume", Source: "myvol", Target: "/vol"}); err != nil {
		t.Fatalf("AddMount volume: %v", err)
	}
	wantVol := []string{"mount", "add", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--type", "volume", "--src", "myvol", "--dst", "/vol", "-cache-dir", "/tmp/cache"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantVol) {
		t.Errorf("AddMount volume args = %q, want %q", got, wantVol)
	}

	// RemoveMount (lazy uses --detach)
	if err := c.RemoveMount(ctx, target, "/guest/dir", true); err != nil {
		t.Fatalf("RemoveMount lazy: %v", err)
	}
	wantRmMnt := []string{"mount", "rm", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--detach", "--", "/guest/dir"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantRmMnt) {
		t.Errorf("RemoveMount lazy args = %q, want %q", got, wantRmMnt)
	}
	if err := c.RemoveMount(ctx, target, "/guest/dir", false); err != nil {
		t.Fatalf("RemoveMount non-lazy: %v", err)
	}
	wantRmMnt2 := []string{"mount", "rm", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--", "/guest/dir"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantRmMnt2) {
		t.Errorf("RemoveMount non-lazy args = %q, want %q", got, wantRmMnt2)
	}

	// Update with NetTXBPS / NetRXBPS
	tx := uint64(1_000_000)
	rx := uint64(0)
	u := Update{NetTXBPS: &tx, NetRXBPS: &rx}
	if u.IsZero() {
		t.Fatal("Update with NetTXBPS/NetRXBPS reported IsZero")
	}
	res, err := c.Update(ctx, target, u)
	if err != nil || res.NetTXBPS != 1_000_000 || res.NetRXBPS != 2_000_000 {
		t.Fatalf("Update bandwidth: %+v %v", res, err)
	}
	wantUpd := []string{"update", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--net-tx-bps", "1000000", "--net-rx-bps", "0", "-f", "json"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantUpd) {
		t.Errorf("Update bandwidth args = %q, want %q", got, wantUpd)
	}

	// Resources JSON unmarshaling with net_tx_bps / net_rx_bps
	var decoded Resources
	if err := json.Unmarshal([]byte(`{"net_tx_bps":123,"net_rx_bps":456}`), &decoded); err != nil || decoded.NetTXBPS != 123 || decoded.NetRXBPS != 456 {
		t.Fatalf("Resources json: %+v %v", decoded, err)
	}

	// NATPolicy get & replace
	raw, err := c.NATPolicy(ctx, target)
	if err != nil || !bytes.Equal(raw, []byte(`{"default":"deny"}`)) {
		t.Fatalf("NATPolicy: %q %v", raw, err)
	}
	wantPolGet := []string{"net-policy", "get", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantPolGet) {
		t.Errorf("NATPolicy args = %q, want %q", got, wantPolGet)
	}

	if err := c.ReplaceNATPolicy(ctx, target, "/etc/policy.yaml"); err != nil {
		t.Fatalf("ReplaceNATPolicy: %v", err)
	}
	wantPolRep := []string{"net-policy", "replace", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--file", "/etc/policy.yaml"}
	if got := readRecordedArgs(t, argsLog); !slices.Equal(got, wantPolRep) {
		t.Errorf("ReplaceNATPolicy args = %q, want %q", got, wantPolRep)
	}
	if err := c.ReplaceNATPolicy(ctx, target, ""); err == nil {
		t.Error("ReplaceNATPolicy accepted empty file")
	}
	if err := c.ReplaceNATPolicy(ctx, target, "-bad.json"); err == nil {
		t.Error("ReplaceNATPolicy accepted leading dash")
	}
}

func TestFeaturesRestoreLiveMountsAndForget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { ForgetFeatures(bin) })

	// Initially: v0.21 run flags, but restore lacks -attach-bind / -serve.
	before := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -control-token-file string"
  echo "  -keep-blobs"
  exit 2
fi
if [ "$1" = "restore" ] && [ "$2" = "-help" ]; then
  echo "  -checkpoint-dir string"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(before), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{Path: bin}
	ctx := context.Background()
	f := c.Features(ctx)
	if !f.Control || !f.KeepBlobs || f.RestoreLiveMounts {
		t.Fatalf("before upgrade Features = %+v, want Control+KeepBlobs without RestoreLiveMounts", f)
	}

	// Upgrade binary to support -attach-bind and -serve on restore; cache still returns false until ForgetFeatures.
	after := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -control-token-file string"
  echo "  -keep-blobs"
  exit 2
fi
if [ "$1" = "restore" ] && [ "$2" = "-help" ]; then
  echo "  -attach-bind value"
  echo "  -checkpoint-dir string"
  echo "  -serve value"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(after), 0o755); err != nil {
		t.Fatal(err)
	}
	if cached := c.Features(ctx); cached.RestoreLiveMounts {
		t.Fatalf("expected cached RestoreLiveMounts=false before ForgetFeatures, got %+v", cached)
	}

	ForgetFeatures(bin)
	f2 := c.Features(ctx)
	if !f2.RestoreLiveMounts {
		t.Fatalf("after ForgetFeatures Features = %+v, want RestoreLiveMounts=true", f2)
	}
	if !c.HasRestoreFlag(ctx, "attach-bind") || !c.HasRestoreFlag(ctx, "serve") || c.HasRestoreFlag(ctx, "nonexistent") {
		t.Fatal("HasRestoreFlag returned unexpected result")
	}

	// Partial restore support (only -attach-bind, missing -serve) keeps RestoreLiveMounts false.
	partial := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -control-token-file string"
  echo "  -keep-blobs"
  exit 2
fi
if [ "$1" = "restore" ] && [ "$2" = "-help" ]; then
  echo "  -attach-bind value"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	ForgetFeatures(bin)
	if f3 := c.Features(ctx); f3.RestoreLiveMounts {
		t.Fatalf("partial restore flags reported RestoreLiveMounts=true: %+v", f3)
	}
}

func TestMountServeAndUnixForwardArgsAndCommands(t *testing.T) {
	c := &Client{Path: "/usr/bin/rungv", CacheDir: "/ignored-for-control"}
	ctx := context.Background()

	// MountServeArgs: rw, ro with token, Windows host path, and no -cache-dir injection.
	rwArgs := c.MountServeArgs(Target{Socket: "/tmp/s.sock"}, "/host/proj", "/mnt/proj", false)
	wantRW := []string{"mount", "serve", "-S", "/tmp/s.sock", "--", "/host/proj:/mnt/proj"}
	if !slices.Equal(rwArgs, wantRW) {
		t.Errorf("MountServeArgs rw = %q, want %q", rwArgs, wantRW)
	}
	roArgs := c.MountServeArgs(Target{Socket: `C:\tmp\s.sock`, TokenFile: `C:\tmp\s.tok`}, `C:\work\proj`, "/mnt/proj", true)
	wantRO := []string{"mount", "serve", "-S", `C:\tmp\s.sock`, "--token-file", `C:\tmp\s.tok`, "--ro", "--", `C:\work\proj:/mnt/proj`}
	if !slices.Equal(roArgs, wantRO) {
		t.Errorf("MountServeArgs ro = %q, want %q", roArgs, wantRO)
	}
	serveCmd := c.MountServeCommand(ctx, Target{Socket: "/tmp/s.sock"}, "/host/proj", "/mnt/proj", true)
	wantServeCmd := []string{"/usr/bin/rungv", "mount", "serve", "-S", "/tmp/s.sock", "--ro", "--", "/host/proj:/mnt/proj"}
	if !slices.Equal(serveCmd.Args, wantServeCmd) {
		t.Errorf("MountServeCommand.Args = %q, want %q", serveCmd.Args, wantServeCmd)
	}

	// UnixForwardArgs: single, multiple with --replace and token, ignores empty specs, no -cache-dir injection.
	ufArgs := c.UnixForwardArgs(Target{Socket: "/tmp/s.sock"}, []string{"/run/host-services/ssh-auth.sock=/tmp/ssh.sock"}, false)
	wantUF := []string{"unix-forward", "-S", "/tmp/s.sock", "--", "/run/host-services/ssh-auth.sock=/tmp/ssh.sock"}
	if !slices.Equal(ufArgs, wantUF) {
		t.Errorf("UnixForwardArgs = %q, want %q", ufArgs, wantUF)
	}
	ufRep := c.UnixForwardArgs(
		Target{Socket: "/tmp/s.sock", TokenFile: "/tmp/s.tok"},
		[]string{"  ", "/run/a.sock=/tmp/a.sock,mode=0600", "/run/b.sock=/tmp/b.sock"},
		true,
	)
	wantUFRep := []string{
		"unix-forward", "-S", "/tmp/s.sock", "--token-file", "/tmp/s.tok", "--replace",
		"--", "/run/a.sock=/tmp/a.sock,mode=0600", "/run/b.sock=/tmp/b.sock",
	}
	if !slices.Equal(ufRep, wantUFRep) {
		t.Errorf("UnixForwardArgs replace = %q, want %q", ufRep, wantUFRep)
	}
	ufCmd := c.UnixForwardCommand(ctx, Target{Socket: "/tmp/s.sock"}, []string{"/run/a.sock=/tmp/a.sock"}, true)
	wantUFCmd := []string{"/usr/bin/rungv", "unix-forward", "-S", "/tmp/s.sock", "--replace", "--", "/run/a.sock=/tmp/a.sock"}
	if !slices.Equal(ufCmd.Args, wantUFCmd) {
		t.Errorf("UnixForwardCommand.Args = %q, want %q", ufCmd.Args, wantUFCmd)
	}
}
