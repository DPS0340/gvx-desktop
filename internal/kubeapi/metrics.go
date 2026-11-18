package kubeapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// PodMetrics is what a pod's containers used over the last window
// metrics-server sampled, as kubectl top pod tells it.
type PodMetrics struct {
	Metadata   Meta               `json:"metadata"`
	Containers []ContainerMetrics `json:"containers"`
}

// ContainerMetrics is what a container of a pod used.
type ContainerMetrics struct {
	Name  string `json:"name"`
	Usage struct {
		CPU    string `json:"cpu"`    // cores, as a quantity: 14072536n
		Memory string `json:"memory"` // bytes, as a quantity: 18592Ki
	} `json:"usage"`
}

// Usage is what a pod or a container uses: CPU in millicores, as kubectl
// top counts it, and its working set of memory in bytes.
type Usage struct {
	MilliCPU float64
	Memory   int64
}

// Add sums two usages.
func (u Usage) Add(o Usage) Usage { return Usage{u.MilliCPU + o.MilliCPU, u.Memory + o.Memory} }

// Key is namespace/name, as a pod's.
func (p PodMetrics) Key() string { return p.Metadata.Namespace + "/" + p.Metadata.Name }

// Used is what a container used; a quantity metrics-server wrote that
// does not parse counts as nothing.
func (c ContainerMetrics) Used() Usage {
	cpu, _ := ParseQuantity(c.Usage.CPU)
	mem, _ := ParseQuantity(c.Usage.Memory)
	return Usage{MilliCPU: cpu * 1000, Memory: int64(math.Round(mem))}
}

// Total is what the pod's containers used together.
func (p PodMetrics) Total() Usage {
	var u Usage
	for _, c := range p.Containers {
		u = u.Add(c.Used())
	}
	return u
}

// PodMetrics lists what every pod uses, from the metrics API that
// metrics-server serves. Without metrics-server, or while it has yet to
// take its first samples, the API server answers with an error: 404 or
// 503.
func (c *Client) PodMetrics(ctx context.Context) ([]PodMetrics, error) {
	var list struct {
		Items []PodMetrics `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/apis/metrics.k8s.io/v1beta1/pods", &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// quantitySuffixes are the multipliers a quantity can end with: binary
// for memory, decimal for both, and the fractions CPU is written in.
var quantitySuffixes = []struct {
	suffix string
	scale  float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60},
	{"n", 1e-9}, {"u", 1e-6}, {"m", 1e-3},
	{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18},
}

// ParseQuantity reads a Kubernetes quantity, such as 250m, 18592Ki or
// 1.5e3, as a number of its base unit: cores for CPU, bytes for memory.
func ParseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	num, scale := s, 1.0
	for _, q := range quantitySuffixes {
		if before, ok := strings.CutSuffix(s, q.suffix); ok {
			num, scale = before, q.scale
			break
		}
	}
	// An exponent, as in 1e3 or 1E3, ends in a digit: ParseFloat reads it.
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || num == "" || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("not a quantity: %q", s)
	}
	return v * scale, nil
}
