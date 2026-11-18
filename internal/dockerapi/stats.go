package dockerapi

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Stats is a sample of what a container uses, as GET
// /containers/{id}/stats answers it.
type Stats struct {
	Read time.Time `json:"read"`
	CPU  CPUStats  `json:"cpu_stats"`
	// PreCPU is the sample before, which the engine fills only when it
	// takes two; a one-shot sample has it empty.
	PreCPU CPUStats    `json:"precpu_stats"`
	Memory MemoryStats `json:"memory_stats"`
}

// CPUStats counts the CPU time a container and the whole machine used.
type CPUStats struct {
	Usage struct {
		Total  uint64   `json:"total_usage"`            // nanoseconds of the container's
		PerCPU []uint64 `json:"percpu_usage,omitempty"` // under cgroup v1 only
	} `json:"cpu_usage"`
	System     uint64 `json:"system_cpu_usage"` // nanoseconds of every CPU's
	OnlineCPUs int    `json:"online_cpus"`
}

// MemoryStats is a container's memory and its limit, in bytes.
type MemoryStats struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

// Used is the memory a container uses as docker stats counts it: less the
// page cache it could give back, which cgroup v2 calls inactive_file and
// v1 total_inactive_file.
func (m MemoryStats) Used() uint64 {
	cache, ok := m.Stats["inactive_file"]
	if !ok {
		cache = m.Stats["total_inactive_file"]
	}
	if cache > m.Usage {
		return m.Usage
	}
	return m.Usage - cache
}

// Stats reads a sample of what a running container uses. With wait the
// engine takes two, a second apart, and fills PreCPU from the first;
// without it the answer comes at once, and CPUPercent compares it with
// the sample read before.
func (c *Client) Stats(ctx context.Context, id string, wait bool) (Stats, error) {
	var s Stats
	q := "?stream=false&one-shot=true"
	if wait {
		q = "?stream=false"
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats"+q, &s)
	return s, err
}

// CPUPercent is the share of a CPU a container used between two samples,
// as docker stats counts it: 100 for one CPU kept busy, more for more. It
// compares cur with its own PreCPU when the engine filled that, and with
// prev otherwise; ok is false when neither is a sample before it.
func CPUPercent(prev, cur Stats) (percent float64, ok bool) {
	before := cur.PreCPU
	if before.System == 0 {
		before = prev.CPU
	}
	if before.System == 0 || cur.CPU.System <= before.System || cur.CPU.Usage.Total < before.Usage.Total {
		return 0, false
	}
	cpus := cur.CPU.OnlineCPUs
	if cpus == 0 {
		cpus = len(cur.CPU.Usage.PerCPU)
	}
	if cpus == 0 {
		cpus = 1
	}
	work := float64(cur.CPU.Usage.Total - before.Usage.Total)
	all := float64(cur.CPU.System - before.System)
	return work / all * float64(cpus) * 100, true
}
