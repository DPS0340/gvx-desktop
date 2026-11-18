package dockerapi

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
)

// statsJSON is a one-shot sample as dockerd 29 in a gvx sandbox answers it:
// cgroup v2, and precpu_stats empty.
const statsJSON = `{"id":"e7c61f1b","name":"/web","read":"2026-11-18T21:41:00.596208147Z",
"cpu_stats":{"cpu_usage":{"total_usage":568936000,"usage_in_kernelmode":10000000,"usage_in_usermode":558936000},
 "system_cpu_usage":4917600000000,"online_cpus":20,"throttling_data":{"periods":0}},
"precpu_stats":{"cpu_usage":{"total_usage":0},"throttling_data":{"periods":0}},
"memory_stats":{"usage":52428800,"stats":{"anon":41943040,"inactive_file":10485760,"active_file":0},"limit":68719476736},
"pids_stats":{"current":2}}`

func TestStats(t *testing.T) {
	var asked string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.RequestURI()
		if r.URL.Path == "/containers/gone/stats" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: gone"}`))
			return
		}
		_, _ = w.Write([]byte(statsJSON))
	})
	if _, err := c.Stats(context.Background(), "web", true); err != nil || asked != "GET /containers/web/stats?stream=false" {
		t.Errorf("asked %s: %v", asked, err)
	}
	s, err := c.Stats(context.Background(), "web", false)
	if err != nil {
		t.Fatal(err)
	}
	if asked != "GET /containers/web/stats?stream=false&one-shot=true" {
		t.Errorf("asked %s", asked)
	}
	if s.CPU.Usage.Total != 568936000 || s.CPU.System != 4917600000000 || s.CPU.OnlineCPUs != 20 || s.PreCPU.System != 0 || s.Read.IsZero() {
		t.Errorf("cpu %+v pre %+v", s.CPU, s.PreCPU)
	}
	if s.Memory.Used() != 40<<20 || s.Memory.Limit != 64<<30 {
		t.Errorf("memory %d of %d", s.Memory.Used(), s.Memory.Limit)
	}
	if _, err := c.Stats(context.Background(), "gone", false); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Errorf("a missing container: %v", err)
	}
}

func TestCPUPercent(t *testing.T) {
	sample := func(total, system uint64, cpus int) Stats {
		var s Stats
		s.CPU.Usage.Total, s.CPU.System, s.CPU.OnlineCPUs = total, system, cpus
		return s
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	// Over about a second of 20 CPUs, 20.2e9 ns of the machine's time, the
	// container used 0.144558 s: 14.3% of one CPU.
	prev, cur := sample(424378000, 4897400000000, 20), sample(568936000, 4917600000000, 20)
	if p, ok := CPUPercent(prev, cur); !ok || !near(p, 14.31267326732673) {
		t.Errorf("against the sample before: %v %v", p, ok)
	}
	withPre := cur
	withPre.PreCPU = sample(0, 4907600000000, 0).CPU
	withPre.PreCPU.Usage.Total = 468936000
	if p, ok := CPUPercent(Stats{}, withPre); !ok || !near(p, 20) {
		t.Errorf("against its own precpu_stats: %v %v", p, ok)
	}
	if _, ok := CPUPercent(Stats{}, cur); ok {
		t.Error("a first one-shot sample has nothing to compare with")
	}
	if _, ok := CPUPercent(cur, cur); ok {
		t.Error("the same sample twice tells nothing")
	}
	if _, ok := CPUPercent(cur, prev); ok {
		t.Error("a restarted container counts from zero again")
	}
	// cgroup v1 tells no online_cpus; its per-CPU usages count them.
	v1prev, v1 := sample(0, 1e9, 0), sample(5e8, 2e9, 0)
	v1.CPU.Usage.PerCPU = []uint64{3e8, 2e8}
	if p, ok := CPUPercent(v1prev, v1); !ok || !near(p, 100) {
		t.Errorf("cgroup v1: %v %v", p, ok)
	}
}
