package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func TestValidateUnixForward(t *testing.T) {
	ok := []string{
		"/run/ssh.sock=/tmp/ssh.sock",
		"/var/run/agent.sock=~/agent.sock,uid=1000,gid=1000,mode=0600",
		"/tmp/s.sock=/host/s.sock,mode=777",
	}
	for _, s := range ok {
		if err := ValidateUnixForward(s); err != nil {
			t.Errorf("ValidateUnixForward(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"",
		"/run/s.sock",
		"relative.sock=/tmp/h.sock",
		"/=/tmp/h.sock",
		"/a/../b=/tmp/h.sock",
		"/" + strings.Repeat("a", 107) + "=/tmp/h.sock",
		"/run/s.sock=",
		"/run/s.sock=/tmp/h.sock,badopt",
		"/run/s.sock=/tmp/h.sock,uid=-1",
		"/run/s.sock=/tmp/h.sock,gid=abc",
		"/run/s.sock=/tmp/h.sock,mode=0",
		"/run/s.sock=/tmp/h.sock,mode=01000",
		"/run/s.sock=/tmp/h.sock,foo=bar",
	}
	for _, s := range bad {
		if err := ValidateUnixForward(s); err == nil {
			t.Errorf("ValidateUnixForward(%q) accepted, want error", s)
		}
	}
}

func TestSpecV021ValidationArgsAndClone(t *testing.T) {
	base := Spec{
		Name:             "app",
		Image:            "alpine:3.20",
		UnixForwards:     []string{"/run/ssh.sock=~/ssh.sock,mode=0600"},
		NetTXBPS:         1_000_000,
		NetRXBPS:         2_000_000,
		NATPolicyFile:    "~/policy.yaml",
		DynamicPortAddrs: []string{"0.0.0.0", "::1"},
		AutoPublishAddr:  "0.0.0.0",
		ExtraArgs:        "-auto-publish",
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid v0.21 spec rejected: %v", err)
	}

	// Clone deep copies new slices.
	cloned := base.Clone()
	cloned.UnixForwards[0] = "/other=/other"
	cloned.DynamicPortAddrs[0] = "127.0.0.1"
	if base.UnixForwards[0] == "/other=/other" || base.DynamicPortAddrs[0] == "127.0.0.1" {
		t.Fatal("Clone shared UnixForwards or DynamicPortAddrs slice")
	}

	// RunArgs & RestoreArgs emit new flags cleanly.
	home, _ := os.UserHomeDir()
	p := Paths{ControlSocket: "/r/s.sock", DynamicPorts: true, AllowBindAttach: true}
	runArgs := base.RunArgs(p)
	for _, pair := range [][]string{
		{"--net-tx-bps", "1000000"},
		{"--net-rx-bps", "2000000"},
		{"--nat-policy-file", filepath.Join(home, "policy.yaml")},
		{"--auto-publish-addr", "0.0.0.0"},
		{"--dynamic-ports"},
		{"--dynamic-ports-addr", "0.0.0.0"},
		{"--dynamic-ports-addr", "::1"},
		{"--unix-forward", "/run/ssh.sock=~/ssh.sock,mode=0600"},
		{"--allow-bind-attach"},
	} {
		if !containsSubslice(runArgs, pair) {
			t.Errorf("RunArgs missing %q in %q", pair, runArgs)
		}
	}
	restoreArgs := base.RestoreArgs("/ck", p)
	for _, pair := range [][]string{
		{"--net-tx-bps", "1000000"},
		{"--net-rx-bps", "2000000"},
		{"--nat-policy-file", filepath.Join(home, "policy.yaml")},
		{"--dynamic-ports"},
		{"--dynamic-ports-addr", "0.0.0.0"},
		{"--unix-forward", "/run/ssh.sock=~/ssh.sock,mode=0600"},
		{"--allow-bind-attach"},
	} {
		if !containsSubslice(restoreArgs, pair) {
			t.Errorf("RestoreArgs missing %q in %q", pair, restoreArgs)
		}
	}
	// RestoreArgs does not pass ExtraArgs (-auto-publish), so it must not emit --auto-publish-addr.
	if slices.Contains(restoreArgs, "--auto-publish-addr") {
		t.Errorf("RestoreArgs unexpectedly emitted --auto-publish-addr: %q", restoreArgs)
	}

	// Validation rejects invalid combinations.
	invalid := []func(*Spec){
		func(s *Spec) {
			s.Network = "none"
			s.Ports = nil
			s.AutoPublishAddr = ""
			s.DynamicPortAddrs = nil
			s.NATPolicyFile = ""
		}, // NetTXBPS on none
		func(s *Spec) {
			s.Network = "none"
			s.NetTXBPS = 0
			s.NetRXBPS = 0
			s.AutoPublishAddr = ""
			s.DynamicPortAddrs = nil
		}, // NATPolicyFile on none
		func(s *Spec) { s.NATPolicyFile = "/tmp/policy.txt" }, // bad extension
		func(s *Spec) {
			s.Network = "none"
			s.NetTXBPS = 0
			s.NetRXBPS = 0
			s.NATPolicyFile = ""
			s.AutoPublishAddr = ""
		}, // DynamicPortAddrs on none
		func(s *Spec) { s.DynamicPortAddrs = []string{"not-an-ip"} },                  // invalid IP
		func(s *Spec) { s.ExtraArgs = "" },                                            // AutoPublishAddr without auto-publish
		func(s *Spec) { s.AutoPublishAddr = "fe80::1%en0" },                           // zoned IP
		func(s *Spec) { s.UnixForwards = []string{"/run/a=/tmp/1", "/run/a=/tmp/2"} }, // duplicate guest path
	}
	for i, mutate := range invalid {
		s := base.Clone()
		mutate(&s)
		if s.Validate() == nil {
			t.Errorf("invalid case %d accepted: %+v", i, s)
		}
	}
}

func containsSubslice(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func TestStateEqualV021Capabilities(t *testing.T) {
	base := State{Status: Running, Control: true, DynamicPorts: true, BindAttach: true, NetQoS: true, NATPolicy: true}
	if !base.Equal(base) {
		t.Fatal("State not equal to itself")
	}
	for name, mut := range map[string]func(*State){
		"DynamicPorts": func(s *State) { s.DynamicPorts = false },
		"BindAttach":   func(s *State) { s.BindAttach = false },
		"NetQoS":       func(s *State) { s.NetQoS = false },
		"NATPolicy":    func(s *State) { s.NATPolicy = false },
	} {
		other := base
		mut(&other)
		if base.Equal(other) {
			t.Errorf("State.Equal ignored %s", name)
		}
	}
}

func TestComputeLiveDiffTable(t *testing.T) {
	base := Spec{
		ID:               "sb1",
		Name:             "app",
		Image:            "alpine:3.20",
		Memory:           "512m",
		CPUs:             "1",
		Ports:            []string{"8080:80"},
		DynamicPortAddrs: []string{"192.168.1.10"},
		Mounts: []Mount{
			{Type: MountVolume, Source: "data", Target: "/data"},
			{Type: MountBind, Source: "/host/a", Target: "/a"},
			{Type: MountTmpfs, Target: "/tmp/s", Size: "32m"},
		},
	}
	fullState := State{
		Control:      true,
		DynamicPorts: true,
		BindAttach:   true,
		NetQoS:       true,
		NATPolicy:    true,
	}

	t.Run("all v0.21 live changes together", func(t *testing.T) {
		ed := base.Clone()
		ed.Memory = "1g"
		ed.NetTXBPS = 5_000_000
		ed.NetRXBPS = 10_000_000
		ed.NATPolicyFile = "/etc/pol.json"
		ed.Ports = []string{"127.0.0.1:9090:90", "192.168.1.10:8443:443"}
		ed.Mounts = []Mount{
			{Type: MountVolume, Source: "data", Target: "/data"},
			{Type: MountBind, Source: "/host/b", Target: "/b"},
			{Type: MountTmpfs, Target: "/tmp/s", Size: "64m"},
		}
		diff, ok := ComputeLiveDiff(base, ed, fullState)
		if !ok {
			t.Fatal("expected live=true")
		}
		if diff.Update.Memory != "1g" || diff.Update.NetTXBPS == nil || *diff.Update.NetTXBPS != 5_000_000 ||
			diff.Update.NetRXBPS == nil || *diff.Update.NetRXBPS != 10_000_000 {
			t.Fatalf("unexpected Update: %+v", diff.Update)
		}
		if diff.NATPolicyFile != "/etc/pol.json" {
			t.Fatalf("NATPolicyFile = %q", diff.NATPolicyFile)
		}
		if !slices.Equal(diff.RemovePorts, []string{"8080:80"}) || !slices.Equal(diff.AddPorts, []string{"127.0.0.1:9090:90", "192.168.1.10:8443:443"}) {
			t.Fatalf("ports diff: add=%q rm=%q", diff.AddPorts, diff.RemovePorts)
		}
		if !slices.Equal(diff.RemoveMounts, []string{"/a", "/tmp/s"}) {
			t.Fatalf("RemoveMounts = %q", diff.RemoveMounts)
		}
		wantAddM := []Mount{
			{Type: MountTmpfs, Target: "/tmp/s", Size: "64m"},
			{Type: MountBind, Source: "/host/b", Target: "/b"},
		}
		if !reflect.DeepEqual(diff.AddMounts, wantAddM) {
			t.Fatalf("AddMounts = %+v, want %+v", diff.AddMounts, wantAddM)
		}
	})

	t.Run("bandwidth gated on State.NetQoS", func(t *testing.T) {
		ed := base.Clone()
		ed.NetTXBPS = 1000
		st := fullState
		st.NetQoS = false
		if _, ok := ComputeLiveDiff(base, ed, st); ok {
			t.Fatal("bandwidth should not be live without State.NetQoS")
		}
	})

	t.Run("NATPolicyFile cleared requires restart", func(t *testing.T) {
		run := base.Clone()
		run.NATPolicyFile = "/etc/pol.json"
		ed := base.Clone()
		if _, ok := ComputeLiveDiff(run, ed, fullState); ok {
			t.Fatal("clearing NATPolicyFile should require restart")
		}
	})

	t.Run("non-loopback unlisted port requires restart", func(t *testing.T) {
		ed := base.Clone()
		ed.Ports = append(ed.Ports, "0.0.0.0:9999:99")
		if _, ok := ComputeLiveDiff(base, ed, fullState); ok {
			t.Fatal("0.0.0.0 port not in DynamicPortAddrs should require restart")
		}
	})

	t.Run("unix port requires restart", func(t *testing.T) {
		ed := base.Clone()
		ed.Ports = append(ed.Ports, "unix:/tmp/a.sock:80")
		if _, ok := ComputeLiveDiff(base, ed, fullState); ok {
			t.Fatal("unix port should require restart")
		}
	})

	t.Run("bind mount requires State.BindAttach", func(t *testing.T) {
		ed := base.Clone()
		ed.Mounts = append(ed.Mounts, Mount{Type: MountBind, Source: "/host/c", Target: "/c"})
		st := fullState
		st.BindAttach = false
		if _, ok := ComputeLiveDiff(base, ed, st); ok {
			t.Fatal("bind mount should not be live without State.BindAttach")
		}
	})

	t.Run("tmpfs mount works with Control only (no BindAttach)", func(t *testing.T) {
		ed := base.Clone()
		ed.Mounts = append(ed.Mounts, Mount{Type: MountTmpfs, Target: "/scratch2", Size: "16m"})
		st := State{Control: true}
		diff, ok := ComputeLiveDiff(base, ed, st)
		if !ok || len(diff.AddMounts) != 1 || diff.AddMounts[0].Target != "/scratch2" {
			t.Fatalf("tmpfs live diff = (%+v, %v)", diff, ok)
		}
	})

	t.Run("volume mount change requires restart", func(t *testing.T) {
		ed := base.Clone()
		ed.Mounts = append(ed.Mounts, Mount{Type: MountVolume, Source: "vol2", Target: "/vol2"})
		if _, ok := ComputeLiveDiff(base, ed, fullState); ok {
			t.Fatal("volume mount should require restart")
		}
	})

	t.Run("LiveUpdate includes NetTXBPS and NetRXBPS", func(t *testing.T) {
		ed := base.Clone()
		ed.NetTXBPS = 100
		ed.NetRXBPS = 200
		u, live := LiveUpdate(base, ed)
		if !live || u.NetTXBPS == nil || *u.NetTXBPS != 100 || u.NetRXBPS == nil || *u.NetRXBPS != 200 {
			t.Fatalf("LiveUpdate = (%+v, %v)", u, live)
		}
	})
}

func TestSupervisorV021FeatureGatingEventsAndApplyLive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake rungv")
	}
	dir := t.TempDir()
	cmdLog := filepath.Join(dir, "cmds.log")
	eventsPipe := filepath.Join(dir, "events.ndjson")
	portsFile := filepath.Join(dir, "ports.ndjson")
	if err := os.WriteFile(portsFile, []byte(`{"proto":"tcp","host_ip":"127.0.0.1","host_port":18080,"guest_ip":"10.0.2.15","guest_port":80,"source":"dynamic"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPipe, []byte(
		`{"timestamp":"2026-11-23T10:00:00Z","type":"port","action":"added"}`+"\n"+
			`{"timestamp":"2026-11-23T10:00:01Z","type":"lifecycle","action":"paused"}`+"\n"+
			`{"timestamp":"2026-11-23T10:00:02Z","type":"oom","action":"killed"}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	binV20 := filepath.Join(dir, "rungv-v20")
	t.Cleanup(func() { rungv.ForgetFeatures(binV20) })
	v20Script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -control-socket string"
  echo "  -control-token-file string"
  echo "  -dynamic-ports"
  exit 2
fi
exec sleep 30
`
	if err := os.WriteFile(binV20, []byte(v20Script), 0o755); err != nil {
		t.Fatal(err)
	}

	// v0.20 rejects v0.21 spec fields with a clear error message.
	sV20, _ := newTestSupervisor(t, &rungv.Client{Path: binV20})
	for _, spec := range []Spec{
		{ID: "u1", Name: "u1", Image: "alpine", UnixForwards: []string{"/run/a=/tmp/a"}},
		{ID: "u2", Name: "u2", Image: "alpine", NetTXBPS: 1000},
		{ID: "u3", Name: "u3", Image: "alpine", NATPolicyFile: "/tmp/p.json"},
		{ID: "u4", Name: "u4", Image: "alpine", DynamicPortAddrs: []string{"0.0.0.0"}},
		{ID: "u5", Name: "u5", Image: "alpine", ExtraArgs: "-auto-publish", AutoPublishAddr: "0.0.0.0"},
	} {
		err := sV20.Start(spec)
		if err == nil || !strings.Contains(err.Error(), "v0.21") {
			t.Errorf("v0.20 Start(%s) = %v, want v0.21 requirement error", spec.ID, err)
		}
	}

	// v0.21 fake rungv: tests State capability flags, watchEvents, and ApplyLive.
	binV21 := filepath.Join(dir, "rungv-v21")
	t.Cleanup(func() { rungv.ForgetFeatures(binV21) })
	v21Script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -allow-bind-attach"
  echo "  -auto-publish-addr string"
  echo "  -control-socket string"
  echo "  -control-token-file string"
  echo "  -dynamic-ports"
  echo "  -dynamic-ports-addr value"
  echo "  -keep-blobs"
  echo "  -nat-policy-file string"
  echo "  -net-rx-bps uint"
  echo "  -net-tx-bps uint"
  echo "  -unix-forward value"
  exit 2
fi
printf '%s\n' "$*" >> "` + cmdLog + `"
cmd="$1"
shift
case "$cmd" in
  run)
    exec sleep 30
    ;;
  events)
    cat "` + eventsPipe + `"
    exec sleep 30
    ;;
  port)
    case "$1" in
      ls) cat "` + portsFile + `"; exit 0 ;;
      add) echo '{"proto":"tcp","host_ip":"127.0.0.1","host_port":19090,"guest_ip":"10.0.2.15","guest_port":90,"source":"dynamic"}'; exit 0 ;;
      rm) exit 0 ;;
    esac
    ;;
  mount)
    case "$1" in
      ls) echo '{"target":"/b","type":"hostfs","source":"/host/b","read_only":false,"mutable":true}'; exit 0 ;;
      add|rm) exit 0 ;;
    esac
    ;;
  update)
    echo '{"memory_bytes":1073741824,"net_tx_bps":5000000,"net_rx_bps":0}'
    exit 0
    ;;
  net-policy)
    case "$1" in
      get) printf '{"default":"deny"}'; exit 0 ;;
      replace) printf '{"default":"allow"}'; exit 0 ;;
    esac
    ;;
esac
exit 0
`
	if err := os.WriteFile(binV21, []byte(v21Script), 0o755); err != nil {
		t.Fatal(err)
	}

	origDelay := syncPortsDelay
	syncPortsDelay = 20 * time.Millisecond
	t.Cleanup(func() { syncPortsDelay = origDelay })

	s, _ := newTestSupervisor(t, &rungv.Client{Path: binV21})
	spec := Spec{
		ID:     "v21sb",
		Name:   "v21-app",
		Image:  "alpine:3.20",
		Memory: "512m",
		Ports:  []string{"8080:80"},
		Mounts: []Mount{{Type: MountBind, Source: "/host/a", Target: "/a"}},
	}
	if err := s.Start(spec); err != nil {
		t.Fatalf("Start v21: %v", err)
	}

	// Wait for watchEvents to process port, pause, and oom events.
	waitFor(t, "v21 events processed", 5*time.Second, func() bool {
		st := s.State(spec.ID)
		return st.Paused && len(st.Ports) == 1 && st.Ports[0].Host == "127.0.0.1:18080" && strings.Contains(st.Advice, "OOM")
	})

	st := s.State(spec.ID)
	if !st.Control || !st.DynamicPorts || !st.BindAttach || !st.NetQoS || !st.NATPolicy {
		t.Fatalf("expected all v0.21 capabilities on State, got %+v", st)
	}
	var sawOOMLog bool
	for _, l := range s.Logs(spec.ID).Snapshot() {
		if l.Stream == System && strings.Contains(l.Text, "── Guest hit memory limit (OOM)") {
			sawOOMLog = true
		}
	}
	if !sawOOMLog {
		t.Fatal("missing OOM system log line")
	}

	// ApplyLive applies Update + NATPolicy + RemovePorts + AddPorts + RemoveMounts + AddMounts.
	edited := spec.Clone()
	edited.Memory = "1g"
	edited.NetTXBPS = 5_000_000
	edited.NATPolicyFile = "/tmp/pol.json"
	edited.Ports = []string{"19090:90"}
	edited.Mounts = []Mount{{Type: MountBind, Source: "/host/b", Target: "/b"}}

	diff, live := ComputeLiveDiff(spec, edited, st)
	if !live || diff.IsZero() {
		t.Fatalf("ComputeLiveDiff = (%+v, %v)", diff, live)
	}
	if err := s.ApplyLive(context.Background(), spec.ID, diff); err != nil {
		t.Fatalf("ApplyLive: %v", err)
	}
	active, ok := s.ActiveSpec(spec.ID)
	if !ok {
		t.Fatal("ActiveSpec returned false")
	}
	if active.Memory != "1g" || active.NetTXBPS != 5_000_000 || active.NATPolicyFile != "/tmp/pol.json" ||
		!slices.Equal(active.Ports, []string{"19090:90"}) ||
		len(active.Mounts) != 1 || active.Mounts[0].Target != "/b" {
		t.Fatalf("ActiveSpec after ApplyLive = %+v", active)
	}

	// Individual helper calls: Mounts, NATPolicy, AddPorts, RemovePorts, AddMount, RemoveMount, ReplaceNATPolicy.
	if mnts, err := s.Mounts(context.Background(), spec.ID); err != nil || len(mnts) != 1 {
		t.Fatalf("Mounts: %+v %v", mnts, err)
	}
	if pol, err := s.NATPolicy(context.Background(), spec.ID); err != nil || string(pol) != `{"default":"deny"}` {
		t.Fatalf("NATPolicy: %q %v", pol, err)
	}
	if err := s.AddMount(context.Background(), spec.ID, Mount{Type: MountTmpfs, Target: "/tmp/extra", Size: "16m"}); err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	if err := s.RemoveMount(context.Background(), spec.ID, "/tmp/extra", true); err != nil {
		t.Fatalf("RemoveMount: %v", err)
	}
	if _, err := s.AddPorts(context.Background(), spec.ID, []string{"19091:91"}, false); err != nil {
		t.Fatalf("AddPorts: %v", err)
	}
	if err := s.RemovePorts(context.Background(), spec.ID, []string{"19091:91"}, false); err != nil {
		t.Fatalf("RemovePorts: %v", err)
	}
	if err := s.ReplaceNATPolicy(context.Background(), spec.ID, "/tmp/pol2.yaml"); err != nil {
		t.Fatalf("ReplaceNATPolicy: %v", err)
	}

	// Verify run received --dynamic-ports and --allow-bind-attach.
	cmds, err := os.ReadFile(cmdLog)
	if err != nil {
		t.Fatal(err)
	}
	cmdStr := string(cmds)
	if !strings.Contains(cmdStr, "--dynamic-ports") || !strings.Contains(cmdStr, "--allow-bind-attach") {
		t.Errorf("run args missing v0.21 flags:\n%s", cmdStr)
	}

	// ContainerID sandbox must NOT get --dynamic-ports or --allow-bind-attach.
	ctrSpec := Spec{ID: "ctr1", Name: "ctr1", Image: "alpine", ContainerID: "deadbeef"}
	if err := s.Start(ctrSpec); err != nil {
		t.Fatalf("Start container: %v", err)
	}
	ctrSt := s.State(ctrSpec.ID)
	if ctrSt.DynamicPorts || ctrSt.BindAttach {
		t.Errorf("container spec should not enable DynamicPorts/BindAttach: %+v", ctrSt)
	}
}
