package ui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/procstat"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Usage is what a sandbox or a container uses: the share of a CPU, as
// docker stats counts it, 100 for one CPU kept busy, and its memory.
type Usage struct {
	CPU    float64
	HasCPU bool  // CPU is known, which takes two samples
	Memory int64 // bytes
	Limit  int64 // the memory it may take, in bytes, or 0 for none known
}

// PodUse is what a pod and each of its containers use, as metrics-server
// last sampled it.
type PodUse struct {
	Total      kubeapi.Usage
	Containers map[string]kubeapi.Usage
}

// Equal makes PodUse comparable for signals.
func (u PodUse) Equal(o PodUse) bool {
	return u.Total == o.Total && maps.Equal(u.Containers, o.Containers)
}

func usageEqual(a, b map[string]Usage) bool { return maps.Equal(a, b) }

func podUsageEqual(a, b map[string]PodUse) bool { return maps.EqualFunc(a, b, PodUse.Equal) }

// podUses makes metrics-server's list into what the Pods page shows, by
// namespace/name.
func podUses(list []kubeapi.PodMetrics) map[string]PodUse {
	out := make(map[string]PodUse, len(list))
	for _, p := range list {
		u := PodUse{Total: p.Total(), Containers: make(map[string]kubeapi.Usage, len(p.Containers))}
		for _, c := range p.Containers {
			u.Containers[c.Name] = c.Used()
		}
		out[p.Key()] = u
	}
	return out
}

// cpuText is a share of a CPU in a few characters: 0.4%, 12%, 230%.
func cpuText(percent float64) string {
	if percent < 9.95 {
		return fmt.Sprintf("%.1f%%", percent)
	}
	return fmt.Sprintf("%.0f%%", percent)
}

// milliCPUText is CPU in millicores, as kubectl top writes it: 3m, 250m;
// a whole core and more in cores, 1.25.
func milliCPUText(m float64) string {
	switch {
	case m >= 1000:
		return fmt.Sprintf("%.2f", m/1000)
	case m > 0 && m < 1:
		return "<1m"
	}
	return fmt.Sprintf("%.0fm", m)
}

// ---- Sandboxes ----------------------------------------------------------

// memoryLimit is a sandbox's memory limit in bytes, as rungv reads one:
// 512m, 2g, 1.5G, bytes for no unit; 0 for none.
func memoryLimit(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	scale := int64(1)
	if i := strings.IndexByte("bkmgt", s[len(s)-1]); i >= 0 {
		scale = int64(1) << (10 * i)
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0
	}
	return int64(v * float64(scale))
}

// sandboxSample is the last sample of a sandbox's process or guest.
type sandboxSample struct {
	pid   int
	s     procstat.Sample
	guest bool
	stats rungv.Stats
}

// guestCPUPercent is the share of a CPU a sandbox's guest used between two
// rungv stats samples: 100 for one CPU kept busy, matching procstat.CPUPercent.
func guestCPUPercent(prev, cur rungv.Stats) (percent float64, ok bool) {
	wall := cur.Timestamp.Sub(prev.Timestamp)
	if prev.Timestamp.IsZero() || cur.Timestamp.IsZero() || wall <= 0 {
		return 0, false
	}
	prevCPU, curCPU := prev.CPUNanos(), cur.CPUNanos()
	if curCPU < prevCPU {
		return 0, false
	}
	return float64(curCPU-prevCPU) / float64(wall.Nanoseconds()) * 100, true
}

// sandboxUsageShown reports whether a page that shows what sandboxes use
// is on screen while one is active (including paused).
func (m *Model) sandboxUsageShown() bool {
	p := m.Page.Get()
	return (p == PageSandboxes || p == PageDashboard) && m.Active.Get() > 0
}

// usageInterval is how often what runs is sampled while a page shows it,
// as often as a page's containers and pods are read.
const usageInterval = 3 * time.Second

// LoadSandboxUsage samples the running sandboxes while a page shows them.
// Sandboxes with a control socket use guest-accurate rungv stats (falling back
// to host procstat on error); others use host procstat. A sandbox with no
// sample before is sampled twice, half a second apart, so its CPU shows at
// once. Asked for while a sampling runs, it does nothing: the next tick is soon.
func (m *Model) LoadSandboxUsage() {
	if m.Offline || m.sandboxUsageLoading {
		return
	}
	if !ggui.Untrack(m.sandboxUsageShown) {
		m.sandboxSamples = nil
		m.SandboxUsage.Set(nil)
		return
	}
	type target struct {
		id  string
		pid int
		ctl sandbox.Control
	}
	var targets []target
	for _, r := range ggui.Peek(m.Rows) {
		if !r.State.Status.Active() {
			continue
		}
		var ctl sandbox.Control
		if r.State.Control {
			ctl, _ = m.sandboxControl(r.Spec.ID)
		}
		if r.State.PID == 0 && ctl == nil {
			continue
		}
		targets = append(targets, target{id: r.Spec.ID, pid: r.State.PID, ctl: ctl})
	}
	prev := maps.Clone(m.sandboxSamples)
	if prev == nil {
		prev = map[string]sandboxSample{}
	}
	m.sandboxUsageLoading = true
	guestGot := map[string]rungv.Stats{}
	var procGot map[int]procstat.Sample
	var procErr error
	m.async(func(ctx context.Context) error {
		// Query guest stats in parallel for rows with a control socket.
		sampleGuests := func(rows []target) map[string]rungv.Stats {
			out := map[string]rungv.Stats{}
			if len(rows) == 0 {
				return out
			}
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, t := range rows {
				wg.Go(func() {
					sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					if st, err := t.ctl.Stats(sctx); err == nil {
						mu.Lock()
						out[t.id] = st
						mu.Unlock()
					}
				})
			}
			wg.Wait()
			return out
		}

		var ctlRows, procRows []target
		for _, t := range targets {
			if t.ctl != nil {
				ctlRows = append(ctlRows, t)
			} else if t.pid != 0 {
				procRows = append(procRows, t)
			}
		}

		var freshCtl []target
		for _, t := range ctlRows {
			if p, ok := prev[t.id]; !ok || p.pid != t.pid {
				freshCtl = append(freshCtl, t)
			}
		}
		procList := make([]int, 0, len(procRows))
		freshProc := false
		for _, t := range procRows {
			procList = append(procList, t.pid)
			if p, ok := prev[t.id]; !ok || p.guest || p.pid != t.pid {
				freshProc = true
			}
		}

		if len(freshCtl) > 0 || freshProc {
			var wg sync.WaitGroup
			var firstGuest map[string]rungv.Stats
			var firstProc map[int]procstat.Sample
			var firstProcErr error
			if len(freshCtl) > 0 {
				wg.Go(func() { firstGuest = sampleGuests(freshCtl) })
			}
			if freshProc {
				wg.Go(func() { firstProc, firstProcErr = procstat.Read(procList) })
			}
			wg.Wait()
			var failedFreshPIDs []int
			for _, t := range freshCtl {
				if st, ok := firstGuest[t.id]; ok {
					prev[t.id] = sandboxSample{pid: t.pid, guest: true, stats: st}
				} else if t.pid != 0 {
					failedFreshPIDs = append(failedFreshPIDs, t.pid)
				}
			}
			if len(failedFreshPIDs) > 0 {
				if fbFirst, err := procstat.Read(failedFreshPIDs); err == nil {
					for _, t := range freshCtl {
						if _, ok := firstGuest[t.id]; ok {
							continue
						}
						if s, ok := fbFirst[t.pid]; ok {
							prev[t.id] = sandboxSample{pid: t.pid, s: s}
						}
					}
				}
			}
			if firstProcErr == nil {
				for _, t := range procRows {
					if s, ok := firstProc[t.pid]; ok && (prev[t.id].guest || prev[t.id].pid != t.pid) {
						prev[t.id] = sandboxSample{pid: t.pid, s: s}
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}

		guestGot = sampleGuests(ctlRows)
		// Any control row whose guest stats failed falls back to procstat.
		fallbackList := slices.Clone(procList)
		for _, t := range ctlRows {
			if _, ok := guestGot[t.id]; !ok && t.pid != 0 {
				fallbackList = append(fallbackList, t.pid)
			}
		}
		if len(fallbackList) > 0 {
			procGot, procErr = procstat.Read(fallbackList)
		}
		return nil
	}, func(error) {
		m.sandboxUsageLoading = false
		if !ggui.Untrack(m.sandboxUsageShown) || (len(guestGot) == 0 && procErr != nil) {
			m.sandboxSamples = nil
			m.SandboxUsage.Set(nil)
			return
		}
		samples := map[string]sandboxSample{}
		usage := map[string]Usage{}
		for _, t := range targets {
			if st, ok := guestGot[t.id]; ok {
				samples[t.id] = sandboxSample{pid: t.pid, guest: true, stats: st}
				u := Usage{Memory: int64(st.Memory.TotalBytes), Limit: int64(st.Memory.LimitBytes)}
				if u.Limit == 0 {
					if spec, ok := m.spec(t.id); ok {
						u.Limit = memoryLimit(spec.Memory)
					}
				}
				if p, ok := prev[t.id]; ok && p.guest && p.pid == t.pid {
					u.CPU, u.HasCPU = guestCPUPercent(p.stats, st)
				}
				usage[t.id] = u
				continue
			}
			if t.pid == 0 {
				continue
			}
			s, ok := procGot[t.pid]
			if !ok {
				continue
			}
			samples[t.id] = sandboxSample{pid: t.pid, s: s}
			u := Usage{Memory: int64(s.Memory)}
			if p, ok := prev[t.id]; ok && !p.guest && p.pid == t.pid {
				u.CPU, u.HasCPU = procstat.CPUPercent(p.s, s)
			}
			if spec, ok := m.spec(t.id); ok {
				u.Limit = memoryLimit(spec.Memory)
			}
			usage[t.id] = u
		}
		m.sandboxSamples = samples
		m.SandboxUsage.Set(usage)
	})
}

// followSandboxUsage samples the running sandboxes while the Sandboxes
// page or the dashboard shows them. Call it from Start.
func (m *Model) followSandboxUsage() {
	if m.Offline {
		return
	}
	every := ggui.Derived(func() time.Duration {
		if m.sandboxUsageShown() {
			return usageInterval
		}
		return 0
	})
	m.poll(every, m.LoadSandboxUsage, m.LoadSandboxUsage)
}

// ---- Containers ---------------------------------------------------------

// containerUsageShown reports whether the Containers page is on screen
// while the engine runs.
func (m *Model) containerUsageShown() bool {
	return m.Page.Get() == PageContainers && m.ContainersOpen.Get()
}

// LoadContainerUsage reads what the engine's running containers use,
// while the Containers page shows them: a container with no sample before
// has the engine take two, so its CPU shows at once. Asked for while a
// reading runs, it does nothing: the next tick is soon.
func (m *Model) LoadContainerUsage() {
	if m.Offline || m.containerUsageLoading {
		return
	}
	if !ggui.Untrack(m.containerUsageShown) {
		m.containerStats = nil
		m.ContainerUsage.Set(nil)
		return
	}
	var ids []string
	for _, c := range ggui.Peek(m.DockerContainers).Items {
		if c.State == "running" || c.State == "paused" {
			ids = append(ids, c.ID)
		}
	}
	prev := m.containerStats
	host := m.dockerBackendHost()
	m.containerUsageLoading = true
	got := map[string]dockerapi.Stats{}
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		var mu sync.Mutex
		var wg sync.WaitGroup
		slots := make(chan struct{}, 8)
		for _, id := range ids {
			_, seen := prev[id]
			wg.Go(func() {
				slots <- struct{}{}
				defer func() { <-slots }()
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				// A container that stopped meanwhile, or is gone, is left out.
				if s, err := c.Stats(ctx, id, !seen); err == nil {
					mu.Lock()
					got[id] = s
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		return nil
	}, func(error) {
		m.containerUsageLoading = false
		if !ggui.Untrack(m.containerUsageShown) {
			m.containerStats = nil
			m.ContainerUsage.Set(nil)
			return
		}
		usage := make(map[string]Usage, len(got))
		for id, s := range got {
			u := Usage{Memory: int64(s.Memory.Used()), Limit: int64(s.Memory.Limit)}
			u.CPU, u.HasCPU = dockerapi.CPUPercent(prev[id], s)
			usage[id] = u
		}
		m.containerStats = got
		m.ContainerUsage.Set(usage)
	})
}

// followContainerUsage reads what the containers use while the Containers
// page shows them, as often as it reads the containers. Call it from
// Start.
func (m *Model) followContainerUsage() {
	if m.Offline {
		return
	}
	every := ggui.Derived(func() time.Duration {
		if m.containerUsageShown() {
			return usageInterval
		}
		return 0
	})
	m.poll(every, m.LoadContainerUsage, m.LoadContainerUsage)
}

// ---- Pods ---------------------------------------------------------------

// wantsPodUsage reports whether what the pods use is read with them: while
// the Pods view shows them and the cluster is open.
func (m *Model) wantsPodUsage() bool {
	return m.Page.Get() == PagePods && m.KubeView.Get() == KubePods && m.PodsOpen.Get()
}

// followPodUsage lists the pods, and with them what they use, as soon as
// the Pods page turns to its Pods view; LoadPods reads it every time after,
// and Go lists them as the page opens. Call it from Start.
func (m *Model) followPodUsage() {
	if m.Offline {
		return
	}
	ggui.Watch(ggui.Map(m.KubeView, func(v int) bool { return v == KubePods }), func(on bool) {
		if on && ggui.Peek(m.Page) == PagePods {
			m.LoadPods()
		}
	})
}

// ---- Showing ------------------------------------------------------------

// usageView is what a page writes of what a row uses: the CPU, the share
// of a CPU its bar fills, the memory, and both in a line for the details.
// Known is false while nothing is.
type usageView struct {
	Known    bool
	CPU, Mem string
	Share    float64
	Line     string
}

// usageOf is what a page writes of a sandbox's or a container's usage.
func usageOf(u Usage, ok bool) usageView {
	if !ok {
		return usageView{CPU: "—", Mem: "—"}
	}
	v := usageView{Known: true, CPU: "—", Mem: humanBytes(u.Memory)}
	if u.HasCPU {
		v.CPU, v.Share = cpuText(u.CPU), min(u.CPU/100, 1)
	}
	mem := v.Mem
	if u.Limit > 0 {
		mem += " / " + humanBytes(u.Limit)
	}
	v.Line = tr("CPU %s · memory %s", v.CPU, mem)
	return v
}

// podUsageOf is what a page writes of a pod's or its container's usage,
// as kubectl top counts it.
func podUsageOf(u kubeapi.Usage, ok bool) usageView {
	if !ok {
		return usageView{CPU: "—", Mem: "—"}
	}
	v := usageView{Known: true, CPU: milliCPUText(u.MilliCPU), Mem: humanBytes(u.Memory), Share: min(u.MilliCPU/1000, 1)}
	v.Line = tr("CPU %s · memory %s", v.CPU, v.Mem)
	return v
}
