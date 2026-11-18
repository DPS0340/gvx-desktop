package ui

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/procstat"
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

// sandboxSample is the last sample of a sandbox's process.
type sandboxSample struct {
	pid int
	s   procstat.Sample
}

// sandboxUsageShown reports whether a page that shows what sandboxes use
// is on screen while one runs.
func (m *Model) sandboxUsageShown() bool {
	p := m.Page.Get()
	return (p == PageSandboxes || p == PageDashboard) && m.Running.Get() > 0
}

// usageInterval is how often what runs is sampled while a page shows it,
// as often as a page's containers and pods are read.
const usageInterval = 3 * time.Second

// LoadSandboxUsage samples the processes of the running sandboxes, while a
// page shows them. A sandbox with no sample before is sampled twice, half
// a second apart, so its CPU shows at once. Asked for while a sampling
// runs, it does nothing: the next tick is soon.
func (m *Model) LoadSandboxUsage() {
	if m.Offline || m.sandboxUsageLoading {
		return
	}
	if !ggui.Untrack(m.sandboxUsageShown) {
		m.sandboxSamples = nil
		m.SandboxUsage.Set(nil)
		return
	}
	pids := map[string]int{}
	for _, r := range ggui.Peek(m.Rows) {
		if r.State.Status.Active() && r.State.PID != 0 {
			pids[r.Spec.ID] = r.State.PID
		}
	}
	prev := maps.Clone(m.sandboxSamples)
	if prev == nil {
		prev = map[string]sandboxSample{}
	}
	m.sandboxUsageLoading = true
	var got map[int]procstat.Sample
	m.async(func(context.Context) error {
		list := make([]int, 0, len(pids))
		fresh := false
		for id, pid := range pids {
			list = append(list, pid)
			if p, ok := prev[id]; !ok || p.pid != pid {
				fresh = true
			}
		}
		if fresh {
			if first, err := procstat.Read(list); err == nil {
				for id, pid := range pids {
					if s, ok := first[pid]; ok && prev[id].pid != pid {
						prev[id] = sandboxSample{pid, s}
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
		var err error
		got, err = procstat.Read(list)
		return err
	}, func(err error) {
		m.sandboxUsageLoading = false
		if err != nil || !ggui.Untrack(m.sandboxUsageShown) {
			m.sandboxSamples = nil
			m.SandboxUsage.Set(nil)
			return
		}
		samples := map[string]sandboxSample{}
		usage := map[string]Usage{}
		for id, pid := range pids {
			s, ok := got[pid]
			if !ok {
				continue
			}
			samples[id] = sandboxSample{pid, s}
			u := Usage{Memory: int64(s.Memory)}
			if p, ok := prev[id]; ok && p.pid == pid {
				u.CPU, u.HasCPU = procstat.CPUPercent(p.s, s)
			}
			if spec, ok := m.spec(id); ok {
				u.Limit = memoryLimit(spec.Memory)
			}
			usage[id] = u
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
		if c.State == "running" {
			ids = append(ids, c.ID)
		}
	}
	prev := m.containerStats
	host := m.dockerHost()
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
// the Pods view shows them.
func (m *Model) wantsPodUsage() bool {
	return m.Page.Get() == PagePods && m.KubeView.Get() == KubePods
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
