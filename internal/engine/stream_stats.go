package engine

// Owner: W2 (originally workstream A; updated for rungv v0.20 guest stats, §5.5).

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/ironpark/gvx-desktop/internal/procstat"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// streamStatsEpoch anchors synthetic system_cpu_usage values so multiplying by
// online_cpus cannot overflow uint64 while preserving exact deltas for
// dockerapi.CPUPercent and docker stats.
var streamStatsEpoch = time.Now().Add(-time.Hour)

type streamStatsPayload struct {
	Read       time.Time                 `json:"read"`
	PreRead    time.Time                 `json:"preread"`
	ID         string                    `json:"id"`
	Name       string                    `json:"name"`
	PidsStats  streamPidsStats           `json:"pids_stats"`
	CPUStats   streamCPUStats            `json:"cpu_stats"`
	PreCPU     streamCPUStats            `json:"precpu_stats"`
	Memory     streamMemoryStats         `json:"memory_stats"`
	Networks   map[string]streamNetStats `json:"networks"`
	BlkioStats streamBlkioStats          `json:"blkio_stats"`
}

type streamPidsStats struct {
	Current uint64 `json:"current,omitempty"`
	Limit   uint64 `json:"limit,omitempty"`
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

type streamBlkioStats struct {
	IOServiceBytesRecursive []streamBlkioEntry `json:"io_service_bytes_recursive"`
	IOServicedRecursive     []streamBlkioEntry `json:"io_serviced_recursive"`
}

type streamBlkioEntry struct {
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

func statsPayloadFromGuest(c Container, gs rungv.Stats, prev streamCPUStats, prevRead time.Time) streamStatsPayload {
	now := gs.Timestamp.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cpus := max(runtime.NumCPU(), 1)
	elapsed := now.Sub(streamStatsEpoch).Nanoseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	sysCPU := uint64(elapsed) * uint64(cpus)

	var cpuTotal uint64
	if cn := gs.CPUNanos(); cn > 0 {
		cpuTotal = uint64(cn)
	}

	memLimit := gs.Memory.LimitBytes
	if memLimit == 0 && c.HostConfig.Memory > 0 {
		memLimit = uint64(c.HostConfig.Memory)
	}
	if memLimit == 0 {
		if hm := hostMemTotal(); hm > 0 {
			memLimit = uint64(hm)
		}
	}

	nets := make(map[string]streamNetStats, len(gs.Network))
	for _, n := range gs.Network {
		if n.Interface == "" || n.Interface == "lo" {
			continue
		}
		nets[n.Interface] = streamNetStats{
			RxBytes:   n.RxBytes,
			RxPackets: n.RxPackets,
			TxBytes:   n.TxBytes,
			TxPackets: n.TxPackets,
		}
	}

	blkioBytes := make([]streamBlkioEntry, 0, len(gs.BlockIO)*2)
	blkioOps := make([]streamBlkioEntry, 0, len(gs.BlockIO)*2)
	for i, b := range gs.BlockIO {
		blkioBytes = append(blkioBytes,
			streamBlkioEntry{Major: 254, Minor: uint64(i), Op: "read", Value: b.ReadSectors * 512},
			streamBlkioEntry{Major: 254, Minor: uint64(i), Op: "write", Value: b.WriteSectors * 512},
		)
		blkioOps = append(blkioOps,
			streamBlkioEntry{Major: 254, Minor: uint64(i), Op: "read", Value: b.ReadOps},
			streamBlkioEntry{Major: 254, Minor: uint64(i), Op: "write", Value: b.WriteOps},
		)
	}

	pids := streamPidsStats{}
	if gs.PIDs > 0 {
		pids.Current = uint64(gs.PIDs)
	}
	if gs.PIDsLimit > 0 {
		pids.Limit = uint64(gs.PIDsLimit)
	}

	return streamStatsPayload{
		Read:      now,
		PreRead:   prevRead,
		ID:        c.ID,
		Name:      "/" + c.Name,
		PidsStats: pids,
		CPUStats: streamCPUStats{
			CPUUsage:   streamCPUUsage{TotalUsage: cpuTotal},
			SystemCPU:  sysCPU,
			OnlineCPUs: cpus,
		},
		PreCPU: prev,
		Memory: streamMemoryStats{
			Usage: gs.Memory.TotalBytes,
			Limit: memLimit,
			Stats: map[string]uint64{
				"anon":          gs.Memory.AnonBytes,
				"file":          gs.Memory.FileBytes,
				"shmem":         gs.Memory.TmpfsBytes,
				"inactive_file": 0,
			},
		},
		Networks: nets,
		BlkioStats: streamBlkioStats{
			IOServiceBytesRecursive: blkioBytes,
			IOServicedRecursive:     blkioOps,
		},
	}
}

func (e *Engine) streamSampleStats(ctx context.Context, c Container, prev streamCPUStats, prevRead time.Time) streamStatsPayload {
	if c.State.Running() {
		if ctl := e.runner.Control(c.ID); ctl != nil {
			sCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			gs, err := ctl.Stats(sCtx)
			cancel()
			if err == nil {
				return statsPayloadFromGuest(c, gs, prev, prevRead)
			}
		}
	}
	return e.streamSampleStatsProcstat(c, prev, prevRead)
}

func (e *Engine) streamSampleStatsProcstat(c Container, prev streamCPUStats, prevRead time.Time) streamStatsPayload {
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
		BlkioStats: streamBlkioStats{
			IOServiceBytesRecursive: []streamBlkioEntry{},
			IOServicedRecursive:     []streamBlkioEntry{},
		},
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
		first := e.streamSampleStats(r.Context(), c, streamCPUStats{}, time.Time{})
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
		second := e.streamSampleStats(r.Context(), c, first.CPUStats, first.Read)
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

	// With Control on a running container, stream directly from one
	// rungv stats subprocess; fall back to procstat on an error before the
	// first sample or without Control.
	if c.State.Running() {
		if ctl := e.runner.Control(c.ID); ctl != nil {
			sCtx, cancel := context.WithCancel(r.Context())
			defer cancel()
			sent := false
			encodeFailed := false
			err := ctl.StreamStats(sCtx, time.Second, func(gs rungv.Stats) {
				if !e.isOpen() {
					cancel()
					return
				}
				cur := c
				if latest, lerr := e.streamLookup(c.ID); lerr == nil {
					cur = latest
				}
				s := statsPayloadFromGuest(cur, gs, prev, prevRead)
				if err := enc.Encode(s); err != nil {
					encodeFailed = true
					cancel()
					return
				}
				if fl != nil {
					fl.Flush()
				}
				sent = true
				prev = s.CPUStats
				prevRead = s.Read
			})
			if encodeFailed || r.Context().Err() != nil || !e.isOpen() {
				return
			}
			if sent && err == nil {
				return
			}
			if sent {
				// Stream ended (for example, container exited): match today's
				// behavior and end cleanly.
				if cur, lerr := e.streamLookup(c.ID); lerr != nil || !cur.State.Running() {
					return
				}
			}
		}
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		cur, err := e.streamLookup(c.ID)
		if err != nil {
			return
		}
		s := e.streamSampleStatsProcstat(cur, prev, prevRead)
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
