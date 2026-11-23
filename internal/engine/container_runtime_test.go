package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestFeaturesCanControlAndInfoWarning(t *testing.T) {
	// v0.19 default engine.
	te19 := newTestEngine(t)
	te19.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"sleep", "10"}})
	cid19 := createAndStartContainer(t, te19, "gvxd-v19", nil)
	if te19.Features().Control || te19.CanControl(cid19) {
		t.Fatalf("v0.19 reported Control=%v CanControl=%v", te19.Features().Control, te19.CanControl(cid19))
	}
	_, b19 := te19.do(t, http.MethodGet, "/info", nil)
	if !strings.Contains(string(b19), "exec, cp, diff, export and commit need rungv 0.20") {
		t.Fatalf("v0.19 /info warnings = %s", b19)
	}

	// v0.20 engine.
	te20, _ := newControlTestEngine(t)
	cid20 := createAndStartContainer(t, te20, "gvxd-v20", nil)
	if !te20.Features().Control || !te20.CanControl(cid20) || !te20.CanControl("gvxd-v20") {
		t.Fatalf("v0.20 Features=%+v CanControl=%v", te20.Features(), te20.CanControl(cid20))
	}
	if te20.CanControl("nonexistent") {
		t.Fatal("CanControl(nonexistent) = true")
	}
	_, b20 := te20.do(t, http.MethodGet, "/info", nil)
	if !strings.Contains(string(b20), `"Warnings":[]`) {
		t.Fatalf("v0.20 /info warnings = %s", b20)
	}
	// Stopped container -> CanControl false.
	_, _ = te20.do(t, http.MethodPost, "/containers/"+cid20+"/stop?t=0", nil)
	if te20.CanControl(cid20) {
		t.Fatal("CanControl(stopped) = true")
	}
}

func TestPauseUnpauseStatusAndInfo(t *testing.T) {
	te, _ := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-pause", nil)

	// Unpause while not paused -> 409.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/unpause", nil); st != http.StatusConflict || !strings.Contains(string(b), "not paused") {
		t.Fatalf("unpause running = %d %s", st, b)
	}

	// Pause -> 204.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/pause", nil); st != http.StatusNoContent {
		t.Fatalf("pause = %d %s", st, b)
	}
	// Second pause -> 409.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/pause", nil); st != http.StatusConflict || !strings.Contains(string(b), "already paused") {
		t.Fatalf("second pause = %d %s", st, b)
	}

	// A zero-State EventChanged (e.g. from log pump) must NOT clear Paused.
	te.containerEvent(sandbox.Event{ID: cid, Kind: sandbox.EventChanged})
	cAfterZero, _ := te.lookup(cid)
	if !cAfterZero.State.Paused {
		t.Fatal("zero-State EventChanged cleared ContainerState.Paused")
	}

	// List shows State:"paused", Status:"Up ... (Paused)", matches status=paused filter.
	_, body := te.do(t, http.MethodGet, `/containers/json?filters={"status":{"paused":true}}`, nil)
	var items []containerListItem
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || items[0].State != StatusPaused || !strings.Contains(items[0].Status, "(Paused)") {
		t.Fatalf("list paused = %+v", items)
	}

	// Inspect shows State.Paused:true, State.Status:"paused", State.Running:true.
	_, body = te.do(t, http.MethodGet, "/containers/"+cid+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(body, &ins)
	if !ins.State.Paused || ins.State.Status != StatusPaused || !ins.State.Running {
		t.Fatalf("inspect paused = %+v", ins.State)
	}

	// /info counts ContainersPaused = 1, ContainersRunning = 0.
	_, body = te.do(t, http.MethodGet, "/info", nil)
	var info struct {
		ContainersRunning int `json:"ContainersRunning"`
		ContainersPaused  int `json:"ContainersPaused"`
	}
	_ = json.Unmarshal(body, &info)
	if info.ContainersPaused != 1 || info.ContainersRunning != 0 {
		t.Fatalf("info counts = %+v", info)
	}

	// Unpause -> 204, state back to running.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/unpause", nil); st != http.StatusNoContent {
		t.Fatalf("unpause = %d %s", st, b)
	}
	_, body = te.do(t, http.MethodGet, "/containers/"+cid+"/json", nil)
	ins = containerInspectResponse{}
	_ = json.Unmarshal(body, &ins)
	if ins.State.Paused || ins.State.Status != StatusRunning {
		t.Fatalf("inspect after unpause = %+v", ins.State)
	}

	// Pause/unpause events emitted.
	te.evt.mu.Lock()
	evs := slices.Clone(te.evt.ring)
	te.evt.mu.Unlock()
	var sawPause, sawUnpause bool
	for _, ev := range evs {
		if ev.ID == cid && ev.Action == "pause" {
			sawPause = true
		}
		if ev.ID == cid && ev.Action == "unpause" {
			sawUnpause = true
		}
	}
	if !sawPause || !sawUnpause {
		t.Errorf("events: pause=%v unpause=%v", sawPause, sawUnpause)
	}
}

func TestTopShape(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-top", nil)

	ctl.topFn = func(ctx context.Context) ([]rungv.Process, error) {
		return []rungv.Process{
			{PID: 1, PPID: 0, UID: 0, CPUNanos: int64(3665 * time.Second), Comm: "sleep", Cmdline: []string{"sleep", "300"}},
			{PID: 7, PPID: 1, UID: 1000, CPUNanos: 0, Comm: "kworker"},
		}, nil
	}
	st, body := te.do(t, http.MethodGet, "/containers/"+cid+"/top?ps_args=aux", nil)
	if st != http.StatusOK {
		t.Fatalf("top = %d %s", st, body)
	}
	var resp containerTopResponse
	_ = json.Unmarshal(body, &resp)
	wantTitles := []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
	if !slices.Equal(resp.Titles, wantTitles) || len(resp.Processes) != 2 {
		t.Fatalf("top = %+v", resp)
	}
	if resp.Processes[0][1] != "1" || resp.Processes[0][6] != "01:01:05" || resp.Processes[0][7] != "sleep 300" {
		t.Errorf("row 0 = %v", resp.Processes[0])
	}
	if resp.Processes[1][0] != "1000" || resp.Processes[1][7] != "[kworker]" {
		t.Errorf("row 1 = %v", resp.Processes[1])
	}
}

func TestContainerUpdateAndWarnings(t *testing.T) {
	te, _ := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-upd", nil)

	// Memory < 6 MiB -> 400.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/update", map[string]any{"Memory": 1024}); st != http.StatusBadRequest || !strings.Contains(string(b), "6MB") {
		t.Fatalf("small mem = %d %s", st, b)
	}

	// Valid update with unsupported field warning + live Runner.Update call.
	st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/update", map[string]any{
		"Memory":        256 * 1024 * 1024,
		"NanoCpus":      1500000000,
		"PidsLimit":     64,
		"CpuShares":     512,
		"RestartPolicy": map[string]any{"Name": "on-failure", "MaximumRetryCount": 3},
	})
	if st != http.StatusOK {
		t.Fatalf("update = %d %s", st, body)
	}
	var resp struct{ Warnings []string }
	_ = json.Unmarshal(body, &resp)
	if !slices.Contains(resp.Warnings, "CpuShares is not supported and was ignored") {
		t.Fatalf("warnings = %v", resp.Warnings)
	}

	te.fake.mu.Lock()
	calls := slices.Clone(te.fake.updateCalls)
	te.fake.mu.Unlock()
	if len(calls) != 1 || calls[0].Update.Memory != "268435456b" || calls[0].Update.CPUs != "1.5" || calls[0].Update.PidsLimit == nil || *calls[0].Update.PidsLimit != 64 {
		t.Fatalf("updateCalls = %+v", calls)
	}

	// Restart picks up updated Spec (Memory, CPUs, -pids-limit in ExtraArgs).
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/restart?t=0", nil); st != http.StatusNoContent {
		t.Fatalf("restart = %d %s", st, b)
	}
	started := te.fake.Started()
	lastSpec := started[len(started)-1]
	if lastSpec.Memory != "268435456b" || lastSpec.CPUs != "1.5" || !strings.Contains(lastSpec.ExtraArgs, "-pids-limit 64") {
		t.Fatalf("restarted spec = %+v", lastSpec)
	}

	// Running without control socket -> warning "apply at the next start".
	te.fake.mu.Lock()
	te.fake.features = rungv.Features{Control: false}
	te.fake.mu.Unlock()
	oldID := createAndStartContainer(t, te, "gvxd-upd-old", nil)
	te.fake.mu.Lock()
	te.fake.features = rungv.Features{Control: true}
	te.fake.mu.Unlock()
	st, body = te.do(t, http.MethodPost, "/containers/"+oldID+"/update", map[string]any{"Memory": 128 * 1024 * 1024})
	if st != http.StatusOK || !strings.Contains(string(body), "next start") {
		t.Fatalf("old run update = %d %s", st, body)
	}

	// AutoRemove container with restart policy -> 400.
	rmID := createAndStartContainer(t, te, "gvxd-upd-rm", map[string]any{
		"HostConfig": map[string]any{"AutoRemove": true},
	})
	if st, _ := te.do(t, http.MethodPost, "/containers/"+rmID+"/update", map[string]any{
		"RestartPolicy": map[string]any{"Name": "always"},
	}); st != http.StatusBadRequest {
		t.Fatalf("AutoRemove + RestartPolicy = %d, want 400", st)
	}
}

func TestKillGuestAndFallbackAndStopPrecedence(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	stopTimeout := 7
	cid := createAndStartContainer(t, te, "gvxd-ks", map[string]any{
		"StopSignal":  "SIGQUIT",
		"StopTimeout": &stopTimeout,
	})

	// 1. Kill with Control delivers guest Kill with signal name.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/kill?signal=TERM", nil); st != http.StatusNoContent {
		t.Fatalf("kill = %d %s", st, b)
	}
	ctl.mu.Lock()
	kills := slices.Clone(ctl.killCalls)
	ctl.mu.Unlock()
	if len(kills) != 1 || kills[0].Signal != "SIGTERM" {
		t.Fatalf("guest kills = %+v", kills)
	}

	// 2. Kill of paused container unpauses first, then sends guest signal.
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/pause", nil)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/kill?signal=USR1", nil); st != http.StatusNoContent {
		t.Fatalf("kill paused = %d %s", st, b)
	}
	if te.fake.State(cid).Paused {
		t.Fatal("container still paused after kill")
	}

	// 3. Unreachable control socket falls back to host signal.
	ctl.killFn = func(ctx context.Context, o rungv.KillOptions) error {
		return &rungv.Error{Code: 1, Stderr: "connect: connection refused"}
	}
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/kill?signal=SIGKILL", nil); st != http.StatusNoContent {
		t.Fatalf("kill fallback = %d %s", st, b)
	}
	p := te.fake.proc(cid)
	if !slices.Contains(p.Signals(), syscall.SIGKILL) {
		t.Fatalf("host fallback signals = %v, want SIGKILL", p.Signals())
	}
	waitFor(t, func() bool {
		c, _ := te.lookup(cid)
		return c.State.Status == StatusExited
	})

	// 4. Stop precedence: Config.StopSignal + Config.StopTimeout, then explicit ?signal= + ?t=, then t<0 -> 24h.
	ctl.killFn = nil
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/stop", nil); st != http.StatusNoContent {
		t.Fatalf("stop default = %d %s", st, b)
	}
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/stop?signal=SIGINT&t=3", nil); st != http.StatusNoContent {
		t.Fatalf("stop explicit = %d %s", st, b)
	}
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/stop?t=-1", nil); st != http.StatusNoContent {
		t.Fatalf("stop negative t = %d %s", st, b)
	}

	te.fake.mu.Lock()
	calls := slices.Clone(te.fake.shutdownCalls)
	te.fake.mu.Unlock()
	if len(calls) < 3 {
		t.Fatalf("shutdownCalls = %+v", calls)
	}
	if calls[0].Signal != "SIGQUIT" || calls[0].Timeout != 7*time.Second {
		t.Errorf("call 0 = %+v, want SIGQUIT 7s", calls[0])
	}
	if calls[1].Signal != "SIGINT" || calls[1].Timeout != 3*time.Second {
		t.Errorf("call 1 = %+v, want SIGINT 3s", calls[1])
	}
	if calls[2].Signal != "SIGQUIT" || calls[2].Timeout != 24*time.Hour {
		t.Errorf("call 2 = %+v, want SIGQUIT 24h", calls[2])
	}
}

func TestGuestStatsAndFallback(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-stats", nil)

	ts := time.Date(2026, 11, 22, 17, 30, 0, 0, time.UTC)
	sample := rungv.Stats{
		Timestamp: ts,
		PIDs:      4,
		PIDsLimit: 64,
	}
	sample.CPU.UserNanos = 1_500_000_000
	sample.CPU.SystemNanos = 500_000_000
	sample.Memory.TotalBytes = 12 << 20
	sample.Memory.AnonBytes = 10 << 20
	sample.Memory.FileBytes = 1 << 20
	sample.Memory.TmpfsBytes = 1 << 20
	sample.Memory.LimitBytes = 512 << 20
	sample.Network = []struct {
		Interface string `json:"interface"`
		RxBytes   uint64 `json:"rx_bytes"`
		TxBytes   uint64 `json:"tx_bytes"`
		RxPackets uint64 `json:"rx_packets"`
		TxPackets uint64 `json:"tx_packets"`
	}{
		{Interface: "lo", RxBytes: 999},
		{Interface: "eth0", RxBytes: 1234, TxBytes: 5678, RxPackets: 10, TxPackets: 20},
	}
	sample.BlockIO = []struct {
		Disk         string `json:"disk"`
		ReadOps      uint64 `json:"read_ops"`
		WriteOps     uint64 `json:"write_ops"`
		ReadSectors  uint64 `json:"read_sectors"`
		WriteSectors uint64 `json:"write_sectors"`
	}{
		{Disk: "vda", ReadSectors: 10, WriteSectors: 4},
	}

	ctl.statsFn = func(ctx context.Context) (rungv.Stats, error) {
		return sample, nil
	}

	// 1. One-shot stats via guest Stats.
	st, body := te.do(t, http.MethodGet, "/containers/"+cid+"/stats?stream=false&one-shot=true", nil)
	if st != http.StatusOK {
		t.Fatalf("stats = %d %s", st, body)
	}
	var got streamStatsPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.CPUStats.CPUUsage.TotalUsage != 2_000_000_000 {
		t.Errorf("cpu total = %d", got.CPUStats.CPUUsage.TotalUsage)
	}
	if got.Memory.Usage != 12<<20 || got.Memory.Limit != 512<<20 || got.Memory.Stats["shmem"] != 1<<20 {
		t.Errorf("memory = %+v", got.Memory)
	}
	if _, hasLo := got.Networks["lo"]; hasLo {
		t.Errorf("networks should exclude lo: %+v", got.Networks)
	}
	if got.Networks["eth0"].RxBytes != 1234 || got.Networks["eth0"].TxBytes != 5678 {
		t.Errorf("eth0 = %+v", got.Networks["eth0"])
	}
	if len(got.BlkioStats.IOServiceBytesRecursive) != 2 || got.BlkioStats.IOServiceBytesRecursive[0].Value != 5120 || got.BlkioStats.IOServiceBytesRecursive[1].Value != 2048 {
		t.Errorf("blkio = %+v", got.BlkioStats)
	}
	if got.PidsStats.Current != 4 || got.PidsStats.Limit != 64 {
		t.Errorf("pids = %+v", got.PidsStats)
	}
	// dockerapi.Stats parses it cleanly.
	var dapi dockerapi.Stats
	if err := json.Unmarshal(body, &dapi); err != nil || dapi.Memory.Used() != 12<<20 {
		t.Fatalf("dockerapi.Stats = %+v err=%v", dapi, err)
	}

	// 2. Streaming stats uses StreamStats.
	ctl.streamFn = func(ctx context.Context, iv time.Duration, fn func(rungv.Stats)) error {
		fn(sample)
		s2 := sample
		s2.Timestamp = ts.Add(time.Second)
		s2.CPU.UserNanos += 500_000_000
		fn(s2)
		return nil
	}
	res, err := te.srv.Client().Get(te.srv.URL + "/containers/" + cid + "/stats?stream=true")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	var lines []streamStatsPayload
	for sc.Scan() {
		var p streamStatsPayload
		if err := json.Unmarshal(sc.Bytes(), &p); err == nil {
			lines = append(lines, p)
		}
	}
	if len(lines) != 2 || lines[1].PreCPU.CPUUsage.TotalUsage != 2_000_000_000 || lines[1].CPUStats.CPUUsage.TotalUsage != 2_500_000_000 {
		t.Fatalf("streamed stats = %+v", lines)
	}

	// 3. Error falls back to procstat (200 OK).
	ctl.statsFn = func(ctx context.Context) (rungv.Stats, error) { return rungv.Stats{}, errors.New("boom") }
	if st, b := te.do(t, http.MethodGet, "/containers/"+cid+"/stats?stream=false&one-shot=true", nil); st != http.StatusOK {
		t.Fatalf("fallback stats = %d %s", st, b)
	}
}

func TestLiveNetworkConnectAndDisconnect(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	for _, n := range []string{"gvxd-n1", "gvxd-n2"} {
		if st, b := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": n}); st != http.StatusCreated {
			t.Fatalf("network create %s = %d %s", n, st, b)
		}
	}

	// Container starting on user network gvxd-n1 joins the overlay at start.
	cid := createAndStartContainer(t, te, "gvxd-live1", map[string]any{
		"HostConfig": map[string]any{"NetworkMode": "gvxd-n1"},
	})
	// Plain container on default bridge does NOT join the overlay.
	plainID := createAndStartContainer(t, te, "gvxd-plain1", nil)

	// Connect running overlay container to gvxd-n2 -> invokes netinit --gvx-apply!
	if st, b := te.do(t, http.MethodPost, "/networks/gvxd-n2/connect", map[string]any{"Container": cid}); st != http.StatusOK {
		t.Fatalf("connect n2 = %d %s", st, b)
	}
	ctl.mu.Lock()
	calls := slices.Clone(ctl.execCalls)
	ctl.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 apply exec call, got %d", len(calls))
	}
	o := calls[0]
	if !slices.Equal(o.Argv, []string{guestcfg.Path, guestcfg.ApplyArg}) || o.User != "0" || o.Privileged == nil || !*o.Privileged {
		t.Fatalf("apply exec opts = %+v", o)
	}
	var enc string
	for _, kv := range o.Env {
		if v, ok := strings.CutPrefix(kv, guestcfg.EnvVar+"="); ok {
			enc = v
		}
	}
	cfg, err := guestcfg.Decode(enc)
	if err != nil || len(cfg.Addresses) != 2 {
		t.Fatalf("apply config = %+v err=%v", cfg, err)
	}

	// Disconnect from gvxd-n2 -> invokes netinit --gvx-apply with 1 address.
	if st, b := te.do(t, http.MethodPost, "/networks/gvxd-n2/disconnect", map[string]any{"Container": cid}); st != http.StatusOK {
		t.Fatalf("disconnect n2 = %d %s", st, b)
	}
	ctl.mu.Lock()
	calls = slices.Clone(ctl.execCalls)
	ctl.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected 2 apply exec calls, got %d", len(calls))
	}
	for _, kv := range calls[1].Env {
		if v, ok := strings.CutPrefix(kv, guestcfg.EnvVar+"="); ok {
			cfg, err = guestcfg.Decode(v)
			if err != nil || len(cfg.Addresses) != 1 {
				t.Fatalf("disconnect apply config = %+v err=%v", cfg, err)
			}
		}
	}

	// Connect bridge-only container -> 200 OK, NO netinit exec (not joined at start).
	if st, b := te.do(t, http.MethodPost, "/networks/gvxd-n2/connect", map[string]any{"Container": plainID}); st != http.StatusOK {
		t.Fatalf("connect plain = %d %s", st, b)
	}
	ctl.mu.Lock()
	callCount := len(ctl.execCalls)
	ctl.mu.Unlock()
	if callCount != 2 {
		t.Fatalf("bridge-only container should not run live apply, got %d calls", callCount)
	}

	// Failure in apply exec -> still returns 200 OK and reverts hub peer addrs.
	prevAddrs, _ := te.ovl.fab.PeerAddrs(cid)
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		return newFakeExecProcOpts(10, false, true, func(p *fakeExecProc) (int, error) {
			return 1, nil
		}), nil
	}
	if st, b := te.do(t, http.MethodPost, "/networks/gvxd-n2/connect", map[string]any{"Container": cid}); st != http.StatusOK {
		t.Fatalf("connect with failed apply = %d %s", st, b)
	}
	afterAddrs, _ := te.ovl.fab.PeerAddrs(cid)
	if !slices.EqualFunc(prevAddrs, afterAddrs, func(a, b netip.Prefix) bool { return a == b }) {
		t.Fatalf("failed apply did not revert peer addrs: prev=%v after=%v", prevAddrs, afterAddrs)
	}
}
