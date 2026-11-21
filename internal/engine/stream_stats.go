package engine

// Owner: workstream A (supervisor stdio, attach, logs, wait, stats).

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/ironpark/gvx-desktop/internal/procstat"
)

// streamStatsEpoch anchors synthetic system_cpu_usage values so multiplying by
// online_cpus cannot overflow uint64 while preserving exact deltas for
// dockerapi.CPUPercent and docker stats.
var streamStatsEpoch = time.Now().Add(-time.Hour)

type streamStatsPayload struct {
	Read      time.Time                 `json:"read"`
	PreRead   time.Time                 `json:"preread"`
	ID        string                    `json:"id"`
	Name      string                    `json:"name"`
	PidsStats streamPidsStats           `json:"pids_stats"`
	CPUStats  streamCPUStats            `json:"cpu_stats"`
	PreCPU    streamCPUStats            `json:"precpu_stats"`
	Memory    streamMemoryStats         `json:"memory_stats"`
	Networks  map[string]streamNetStats `json:"networks"`
}

type streamPidsStats struct {
	Current uint64 `json:"current,omitempty"`
}

type streamCPUStats struct {
	CPUUsage   streamCPUUsage `json:"cpu_usage"`
	SystemCPU  uint64         `json:"system_cpu_usage"`
	OnlineCPUs int            `json:"online_cpus"`
}

type streamCPUUsage struct {
	TotalUsage uint64 `json:"total_usage"`
}

type streamMemoryStats struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

type streamNetStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxBytes   uint64 `json:"tx_bytes"`
	TxPackets uint64 `json:"tx_packets"`
}

func (e *Engine) streamSampleStats(c Container, prev streamCPUStats, prevRead time.Time) streamStatsPayload {
	now := time.Now().UTC()
	cpus := max(runtime.NumCPU(), 1)
	elapsed := now.Sub(streamStatsEpoch).Nanoseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	sysCPU := uint64(elapsed) * uint64(cpus)

	var (
		cpuTotal uint64
		memUsage uint64
		pids     uint64
	)
	pid := c.State.Pid
	if pid == 0 {
		pid = e.runner.State(c.ID).PID
	}
	if pid > 0 && c.State.Running() {
		pids = 1
		if samples, err := procstat.Read([]int{pid}); err == nil {
			if s, ok := samples[pid]; ok {
				if s.CPU > 0 {
					cpuTotal = uint64(s.CPU.Nanoseconds())
				}
				memUsage = s.Memory
				if !s.At.IsZero() {
					now = s.At.UTC()
					if el := now.Sub(streamStatsEpoch).Nanoseconds(); el > 0 {
						sysCPU = uint64(el) * uint64(cpus)
					}
				}
			}
		}
	}

	var memLimit uint64
	if c.HostConfig.Memory > 0 {
		memLimit = uint64(c.HostConfig.Memory)
	}
	return streamStatsPayload{
		Read:      now,
		PreRead:   prevRead,
		ID:        c.ID,
		Name:      "/" + c.Name,
		PidsStats: streamPidsStats{Current: pids},
		CPUStats: streamCPUStats{
			CPUUsage:   streamCPUUsage{TotalUsage: cpuTotal},
			SystemCPU:  sysCPU,
			OnlineCPUs: cpus,
		},
		PreCPU: prev,
		Memory: streamMemoryStats{
			Usage: memUsage,
			Limit: memLimit,
			Stats: map[string]uint64{},
		},
		Networks: map[string]streamNetStats{},
	}
}

func (e *Engine) containerStats(w http.ResponseWriter, r *http.Request) {
	c, err := e.streamLookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	stream := true
	if r.URL.Query().Has("stream") {
		stream = queryBool(r, "stream")
	}
	oneShot := queryBool(r, "one-shot")

	if !stream {
		first := e.streamSampleStats(c, streamCPUStats{}, time.Time{})
		if oneShot || !c.State.Running() {
			writeJSON(w, http.StatusOK, first)
			return
		}
		t := time.NewTimer(time.Second)
		defer t.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
		if cur, err := e.streamLookup(c.ID); err == nil {
			c = cur
		}
		second := e.streamSampleStats(c, first.CPUStats, first.Read)
		writeJSON(w, http.StatusOK, second)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	var (
		prev     streamCPUStats
		prevRead time.Time
	)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		cur, err := e.streamLookup(c.ID)
		if err != nil {
			return
		}
		s := e.streamSampleStats(cur, prev, prevRead)
		if err := enc.Encode(s); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		prev = s.CPUStats
		prevRead = s.Read
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !e.isOpen() {
				return
			}
		}
	}
}
