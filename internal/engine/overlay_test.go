package engine

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/overlay"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestWithOverlayRunsNetinitInFront(t *testing.T) {
	spec := sandbox.Spec{
		ID:            "c1",
		Image:         "alpine",
		SetEntrypoint: true,
		Entrypoint:    "/docker-entrypoint.sh",
		Command:       rungv.FormatArgs([]string{"nginx", "-g", "daemon off;"}),
		Env:           []string{"A=1", "HOME=/data", "GVX_NET=stale"},
		User:          "app:app",
		Mounts:        []sandbox.Mount{{Type: sandbox.MountVolume, Source: "v", Target: "/v"}},
		ExtraArgs:     "-read-only",
	}
	cfg := guestcfg.Config{PrivateKey: make([]byte, 32), HubPublicKey: make([]byte, 32), Endpoint: "10.0.2.2:5000", Addresses: []string{"10.89.1.2/24"}}
	// Extra hosts alone: netinit runs, with no DNS server.
	hostsOnly, err := overlay.WithNetinit(spec, guestcfg.Config{Hosts: []string{"10.0.0.5\tdb"}}, "/data/netinit.d", netip.AddrPort{})
	if err != nil || hostsOnly.ExtraArgs != "-read-only" || hostsOnly.Entrypoint != guestcfg.Path {
		t.Fatalf("hosts only = %+v, %v", hostsOnly, err)
	}

	out, err := overlay.WithNetinit(spec, cfg, "/data/netinit.d", netip.MustParseAddrPort("127.0.0.1:5353"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Entrypoint != guestcfg.Path || !out.SetEntrypoint {
		t.Fatalf("entrypoint = %q", out.Entrypoint)
	}
	args, _ := rungv.SplitArgs(out.Command)
	if !slices.Equal(args, []string{"/docker-entrypoint.sh", "nginx", "-g", "daemon off;"}) {
		t.Fatalf("command = %q", args)
	}
	if out.User != overlay.NetinitUser {
		t.Fatalf("user = %q, want root named outright for netinit", out.User)
	}
	var enc []string
	for _, kv := range out.Env {
		if v, ok := strings.CutPrefix(kv, guestcfg.EnvVar+"="); ok {
			enc = append(enc, v)
		}
	}
	if len(enc) != 1 {
		t.Fatalf("env = %q", out.Env)
	}
	got, err := guestcfg.Decode(enc[0])
	if err != nil || got.User != "app:app" || !got.KeepHome || got.Endpoint != "10.0.2.2:5000" {
		t.Fatalf("config = %+v, %v", got, err)
	}
	last := out.Mounts[len(out.Mounts)-1]
	if last.Source != "/data/netinit.d" || last.Target != guestcfg.MountDir || !last.ReadOnly || last.Type != sandbox.MountBind {
		t.Fatalf("mount = %+v", last)
	}
	if out.ExtraArgs != "-read-only -host-dns 127.0.0.1:5353" {
		t.Fatalf("extra args = %q", out.ExtraArgs)
	}
	// The container's own record is untouched.
	if spec.User != "app:app" || len(spec.Mounts) != 1 || spec.Entrypoint != "/docker-entrypoint.sh" {
		t.Fatalf("spec changed: %+v", spec)
	}
}

func TestContainersOnUserNetworksJoinTheOverlay(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:latest", ImageConfig{Cmd: []string{"sh"}})
	if st, body := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "gvxd-app"}); st != http.StatusCreated {
		t.Fatalf("network create = %d %s", st, body)
	}
	mustCreate := func(name string, body map[string]any) containerCreateResponse {
		t.Helper()
		st, c, raw := createTestContainer(t, te, name, body)
		if st != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, st, raw)
		}
		return c
	}
	web := mustCreate("gvxd-web", map[string]any{
		"Image": "alpine", "Cmd": []string{"sleep", "100"}, "User": "nobody",
		"HostConfig":       map[string]any{"NetworkMode": "gvxd-app"},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"gvxd-app": map[string]any{"Aliases": []string{"web"}}}},
	})
	db := mustCreate("gvxd-db", map[string]any{
		"Image": "alpine", "Cmd": []string{"sleep", "100"},
		"HostConfig":       map[string]any{"NetworkMode": "gvxd-app"},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"gvxd-app": map[string]any{"IPAMConfig": map[string]any{"IPv4Address": "10.89.1.50"}}}},
	})
	plain := mustCreate("gvxd-plain", map[string]any{"Image": "alpine", "Cmd": []string{"sleep", "100"}})
	// An image whose USER is not root, as many are (USER app).
	te.fake.addImage("unprivileged:latest", ImageConfig{Cmd: []string{"sh"}, User: "app"})
	api := mustCreate("gvxd-api", map[string]any{
		"Image": "unprivileged", "Cmd": []string{"sleep", "100"},
		"HostConfig": map[string]any{"NetworkMode": "gvxd-app"},
	})
	for _, id := range []string{web.ID, db.ID, plain.ID, api.ID} {
		if st, body := te.do(t, http.MethodPost, "/containers/"+id+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start = %d %s", st, body)
		}
	}

	specs := map[string]sandbox.Spec{}
	for _, s := range te.fake.Started() {
		specs[s.ID] = s
	}
	cfgOf := func(s sandbox.Spec) guestcfg.Config {
		t.Helper()
		for _, kv := range s.Env {
			if v, ok := strings.CutPrefix(kv, guestcfg.EnvVar+"="); ok {
				c, err := guestcfg.Decode(v)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
		}
		t.Fatalf("%s has no overlay config: %+v", s.Name, s)
		return guestcfg.Config{}
	}
	wcfg := cfgOf(specs[web.ID])
	if !slices.Equal(wcfg.Addresses, []string{"10.89.1.2/24"}) || wcfg.User != "nobody" || specs[web.ID].User != overlay.NetinitUser {
		t.Fatalf("web config = %+v, user %q", wcfg, specs[web.ID].User)
	}
	// The image's USER goes to netinit to drop to, and the sandbox is still
	// started as root: given no user, rungv would start it as the image's
	// USER, and netinit could then set up nothing.
	if acfg := cfgOf(specs[api.ID]); acfg.User != "app" || specs[api.ID].User != overlay.NetinitUser {
		t.Fatalf("api config = %+v, sandbox user %q", acfg, specs[api.ID].User)
	}
	if !strings.Contains(specs[web.ID].ExtraArgs, "-host-dns 127.0.0.1:") || !strings.Contains(specs[web.ID].ExtraArgs, "-tune net.nat.mtu=60080") || wcfg.MTU != 60000 {
		t.Fatalf("web extra args = %q, mtu %d", specs[web.ID].ExtraArgs, wcfg.MTU)
	}
	if dcfg := cfgOf(specs[db.ID]); !slices.Equal(dcfg.Addresses, []string{"10.89.1.50/24"}) {
		t.Fatalf("db addresses = %v", dcfg.Addresses)
	}
	if p := specs[plain.ID]; p.Entrypoint == guestcfg.Path || strings.Contains(p.ExtraArgs, "host-dns") {
		t.Fatalf("a container on the default bridge joined the overlay: %+v", p)
	}
	// Verify the netinit mount is a read-only directory holding netinit + .gvx_meta.
	var netMount *sandbox.Mount
	for i := range specs[web.ID].Mounts {
		if specs[web.ID].Mounts[i].Target == guestcfg.MountDir {
			netMount = &specs[web.ID].Mounts[i]
			break
		}
	}
	if netMount == nil || netMount.Type != sandbox.MountBind || !netMount.ReadOnly {
		t.Fatalf("web netinit mount = %+v", netMount)
	}
	for _, name := range []string{guestcfg.BinaryName, ".gvx_meta"} {
		if _, err := os.Stat(filepath.Join(netMount.Source, name)); err != nil {
			t.Fatalf("netinit mount dir missing %s: %v", name, err)
		}
	}

	fab := te.ovl.fab
	if fab == nil {
		t.Fatal("no overlay")
	}
	web2 := netip.MustParseAddr("10.89.1.2")
	db50 := netip.MustParseAddr("10.89.1.50")
	if !fab.CanReach(web2, db50) {
		t.Fatal("web cannot reach db")
	}

	// The network shows the subnet and the addresses.
	_, body := te.do(t, http.MethodGet, "/networks/gvxd-app", nil)
	var n dockerNetwork
	_ = json.Unmarshal(body, &n)
	if len(n.IPAM.Config) != 1 || n.IPAM.Config[0].Subnet != "10.89.1.0/24" || n.Containers[db.ID].IPv4Address != "10.89.1.50/24" {
		t.Fatalf("network = %s", body)
	}

	// db stops: web can no longer reach it.
	if st, body := te.do(t, http.MethodPost, "/containers/"+db.ID+"/stop?t=0", nil); st != http.StatusNoContent {
		t.Fatalf("stop = %d %s", st, body)
	}
	waitFor(t, func() bool { return !fab.CanReach(web2, db50) })

	// Disconnecting web from the network updates its record.
	if st, body := te.do(t, http.MethodPost, "/networks/gvxd-app/disconnect", map[string]any{"Container": web.ID}); st != http.StatusOK {
		t.Fatalf("disconnect = %d %s", st, body)
	}
	_, body = te.do(t, http.MethodGet, "/containers/"+web.ID+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(body, &ins)
	if _, ok := ins.NetworkSettings.Networks["gvxd-app"]; ok {
		t.Fatalf("web still shows gvxd-app: %s", body)
	}
	// And connecting it back gives it an address again.
	if st, body := te.do(t, http.MethodPost, "/networks/gvxd-app/connect", map[string]any{"Container": web.ID}); st != http.StatusOK {
		t.Fatalf("connect = %d %s", st, body)
	}
	_, body = te.do(t, http.MethodGet, "/containers/"+web.ID+"/json", nil)
	ins = containerInspectResponse{}
	_ = json.Unmarshal(body, &ins)
	if ep := ins.NetworkSettings.Networks["gvxd-app"]; ep.IPAddress == "" || ep.Gateway != "10.89.1.1" {
		t.Fatalf("web after connect = %+v", ep)
	}
	if st, _ := te.do(t, http.MethodPost, "/networks/gvxd-app/connect", map[string]any{"Container": web.ID}); st != http.StatusForbidden {
		t.Fatalf("second connect = %d, want 403", st)
	}
}

func startedConfig(t *testing.T, te *testEngine, id string) (sandbox.Spec, guestcfg.Config, bool) {
	t.Helper()
	var spec sandbox.Spec
	found := false
	for _, s := range te.fake.Started() {
		if s.ID == id {
			spec, found = s, true
		}
	}
	if !found {
		t.Fatalf("%s did not start", id)
	}
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, guestcfg.EnvVar+"="); ok {
			c, err := guestcfg.Decode(v)
			if err != nil {
				t.Fatal(err)
			}
			return spec, c, true
		}
	}
	return spec, guestcfg.Config{}, false
}

func TestExtraHostsAndInternalNetworks(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:latest", ImageConfig{Cmd: []string{"sh"}})
	for _, n := range []map[string]any{{"Name": "gvxd-int", "Internal": true}, {"Name": "gvxd-pub"}} {
		if st, body := te.do(t, http.MethodPost, "/networks/create", n); st != http.StatusCreated {
			t.Fatalf("network create = %d %s", st, body)
		}
	}
	create := func(name string, body map[string]any) string {
		t.Helper()
		st, c, raw := createTestContainer(t, te, name, body)
		if st != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, st, raw)
		}
		if st, raw := te.do(t, http.MethodPost, "/containers/"+c.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start %s = %d %s", name, st, raw)
		}
		return c.ID
	}
	hosts := create("gvxd-hosts", map[string]any{"Image": "alpine", "HostConfig": map[string]any{
		"ExtraHosts": []string{"db:10.0.0.5", "host.docker.internal:host-gateway"}}})
	internal := create("gvxd-internal", map[string]any{"Image": "alpine", "HostConfig": map[string]any{"NetworkMode": "gvxd-int"}})
	both := create("gvxd-both", map[string]any{"Image": "alpine", "HostConfig": map[string]any{"NetworkMode": "gvxd-int"},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"gvxd-int": map[string]any{}, "gvxd-pub": map[string]any{}}}})

	spec, cfg, ok := startedConfig(t, te, hosts)
	if !ok || cfg.HasLink() || !slices.Equal(cfg.Hosts, []string{"10.0.0.5\tdb", "10.0.2.2\thost.docker.internal"}) || strings.Contains(spec.ExtraArgs, "host-dns") {
		t.Fatalf("extra hosts on the bridge = %+v (%v), extra args %q", cfg, ok, spec.ExtraArgs)
	}
	if _, cfg, _ := startedConfig(t, te, internal); !cfg.Isolated || !cfg.HasLink() {
		t.Fatalf("internal = %+v", cfg)
	}
	if _, cfg, _ := startedConfig(t, te, both); cfg.Isolated || len(cfg.Addresses) != 2 {
		t.Fatalf("internal and public = %+v", cfg)
	}

	if st, raw, _ := createTestContainer(t, te, "gvxd-bad", map[string]any{"Image": "alpine", "HostConfig": map[string]any{"ExtraHosts": []string{"nope"}}}); st != http.StatusBadRequest {
		t.Fatalf("bad extra host = %d %+v", st, raw)
	}
}

// A rungv without -tune keeps eth0 at 1500, so the link stays at 1420.
func TestOverlayWithoutTune(t *testing.T) {
	te := newTestEngine(t)
	te.fake.NoRunFlags = []string{"tune"}
	te.fake.addImage("alpine:latest", ImageConfig{Cmd: []string{"sh"}})
	te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "gvxd-nt"})
	_, c, raw := createTestContainer(t, te, "gvxd-nt1", map[string]any{"Image": "alpine", "HostConfig": map[string]any{"NetworkMode": "gvxd-nt"}})
	if c.ID == "" {
		t.Fatalf("create: %s", raw)
	}
	te.do(t, http.MethodPost, "/containers/"+c.ID+"/start", nil)
	spec, cfg, ok := startedConfig(t, te, c.ID)
	if !ok || cfg.MTU != 1420 || strings.Contains(spec.ExtraArgs, "-tune") {
		t.Fatalf("mtu %d, extra args %q", cfg.MTU, spec.ExtraArgs)
	}
}

// A restart's join is not undone by the exit of the run before it.
func TestRestartKeepsTheNetwork(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:latest", ImageConfig{Cmd: []string{"sh"}})
	if st, body := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "gvxd-r"}); st != http.StatusCreated {
		t.Fatalf("network create = %d %s", st, body)
	}
	var ids []string
	for _, name := range []string{"gvxd-r1", "gvxd-r2"} {
		_, c, raw := createTestContainer(t, te, name, map[string]any{"Image": "alpine", "HostConfig": map[string]any{"NetworkMode": "gvxd-r"}})
		if c.ID == "" {
			t.Fatalf("create: %s", raw)
		}
		te.do(t, http.MethodPost, "/containers/"+c.ID+"/start", nil)
		ids = append(ids, c.ID)
	}
	// The exit's handling stalls after the record stopped running, so the
	// restart's start runs inside it.
	var stalled atomic.Bool
	testHookExitRecorded = func() {
		if stalled.CompareAndSwap(false, true) {
			time.Sleep(1500 * time.Millisecond)
		}
	}
	t.Cleanup(func() { testHookExitRecorded = nil })
	a, b := netip.MustParseAddr("10.89.1.2"), netip.MustParseAddr("10.89.1.3")
	for i := range 3 {
		if st, raw := te.do(t, http.MethodPost, "/containers/"+ids[0]+"/restart?t=0", nil); st != http.StatusNoContent {
			t.Fatalf("restart %d = %d %s", i, st, raw)
		}
		time.Sleep(1600 * time.Millisecond) // let a stalled exit finish
		if !te.ovl.fab.CanReach(a, b) {
			t.Fatalf("after restart %d, the container lost its network", i)
		}
	}
}

func TestNetworkSubnets(t *testing.T) {
	te := newTestEngine(t)
	create := func(body map[string]any) (int, string) {
		st, b := te.do(t, http.MethodPost, "/networks/create", body)
		return st, string(b)
	}
	if st, b := create(map[string]any{"Name": "gvxd-a"}); st != http.StatusCreated {
		t.Fatalf("a = %d %s", st, b)
	}
	if st, b := create(map[string]any{"Name": "gvxd-b", "IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "172.28.0.0/16", "Gateway": "172.28.5.254"}}}}); st != http.StatusCreated {
		t.Fatalf("b = %d %s", st, b)
	}
	if st, b := create(map[string]any{"Name": "gvxd-c", "IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "172.28.3.0/24"}}}}); st != http.StatusForbidden {
		t.Fatalf("overlapping c = %d %s", st, b)
	}
	if st, b := create(map[string]any{"Name": "gvxd-d"}); st != http.StatusCreated {
		t.Fatalf("d = %d %s", st, b)
	}
	a, _ := te.network("gvxd-a")
	b, _ := te.network("gvxd-b")
	d, _ := te.network("gvxd-d")
	if a.Subnet != "10.89.1.0/24" || a.Gateway != "10.89.1.1" || b.Subnet != "172.28.0.0/16" || b.Gateway != "172.28.5.254" || d.Subnet != "10.89.2.0/24" {
		t.Fatalf("subnets: a %s %s, b %s %s, d %s", a.Subnet, a.Gateway, b.Subnet, b.Gateway, d.Subnet)
	}
	// A taken or outside address is refused.
	ep, err := te.connectNetwork("gvxd-b", strings.Repeat("1", 64), nil, "172.28.0.9")
	if err != nil || ep.IPAddress != "172.28.0.9" || ep.IPPrefixLen != 16 {
		t.Fatalf("static = %+v %v", ep, err)
	}
	if _, err := te.connectNetwork("gvxd-b", strings.Repeat("2", 64), nil, "172.28.0.9"); err == nil {
		t.Fatal("a taken address was given twice")
	}
	if _, err := te.connectNetwork("gvxd-b", strings.Repeat("2", 64), nil, "10.0.0.1"); err == nil {
		t.Fatal("an address outside the subnet was given")
	}
	// The gateway is elsewhere, so the first address is free.
	ep, err = te.connectNetwork("gvxd-b", strings.Repeat("2", 64), nil, "")
	if err != nil || ep.IPAddress != "172.28.0.1" {
		t.Fatalf("next = %+v %v", ep, err)
	}
}

func TestNetworksFromBeforeTheOverlayGetSubnets(t *testing.T) {
	dir := t.TempDir()
	cid := strings.Repeat("a", 64)
	old := `{"version":1,"networks":[{"id":"` + strings.Repeat("e", 64) + `","name":"gvxd-old","driver":"bridge","created":"2026-01-01T00:00:00Z","containers":{"` + cid + `":["web"]}}]}`
	if err := os.WriteFile(filepath.Join(dir, "networks.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	te := reopenTestEngine(t, dir, newFakeRunner())
	n, err := te.network("gvxd-old")
	if err != nil || n.Subnet != "10.89.1.0/24" || n.IPs[cid] != "10.89.1.2" {
		t.Fatalf("old network = %+v, %v", n, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
