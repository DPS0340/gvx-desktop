package kubeapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
)

// podMetricsJSON is what k3s's metrics-server answers, cut down to two
// pods, one of two containers.
const podMetricsJSON = `{"kind":"PodMetricsList","apiVersion":"metrics.k8s.io/v1beta1","metadata":{},"items":[
{"metadata":{"name":"coredns-ccb96694c-n8pbd","namespace":"kube-system","creationTimestamp":"2026-11-18T21:41:08Z","labels":{"k8s-app":"kube-dns"}},
 "timestamp":"2026-11-18T21:40:51Z","window":"17.798s","containers":[{"name":"coredns","usage":{"cpu":"14072536n","memory":"18592Ki"}}]},
{"metadata":{"name":"web-7d4b9c8f6d-x7k2p","namespace":"default"},"timestamp":"2026-11-18T21:40:50Z","window":"14.784s",
 "containers":[{"name":"web","usage":{"cpu":"2m","memory":"30Mi"}},{"name":"exporter","usage":{"cpu":"500u","memory":"1048576"}}]}]}`

func TestPodMetrics(t *testing.T) {
	dns, ips := k3sNames()
	ready := true
	var asked string
	k := newCluster(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.RequestURI()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","message":"the server is currently unable to handle the request (get pods.metrics.k8s.io)","reason":"ServiceUnavailable","code":503}`))
			return
		}
		_, _ = w.Write([]byte(podMetricsJSON))
	}, dns, ips)
	c := connect(t, k.entry)
	list, err := c.PodMetrics(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("%d pods, %v", len(list), err)
	}
	if asked != "GET /apis/metrics.k8s.io/v1beta1/pods" {
		t.Errorf("asked %s", asked)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	dnsUse, web := list[0], list[1]
	if u := dnsUse.Total(); dnsUse.Key() != "kube-system/coredns-ccb96694c-n8pbd" || !near(u.MilliCPU, 14.072536) || u.Memory != 18592*1024 {
		t.Errorf("coredns %s %+v", dnsUse.Key(), u)
	}
	if u := web.Containers[1].Used(); !near(u.MilliCPU, 0.5) || u.Memory != 1<<20 {
		t.Errorf("exporter %+v", u)
	}
	if u := web.Total(); !near(u.MilliCPU, 2.5) || u.Memory != 31<<20 {
		t.Errorf("web %+v", u)
	}

	ready = false
	_, err = c.PodMetrics(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Code != http.StatusServiceUnavailable {
		t.Errorf("metrics-server not ready: %v", err)
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]float64{
		"14072536n": 0.014072536,
		"250m":      0.25,
		"500u":      0.0005,
		"2":         2,
		"1.5":       1.5,
		"18592Ki":   18592 * 1024,
		"30Mi":      30 << 20,
		"2Gi":       2 << 30,
		"1k":        1000,
		"128M":      128e6,
		"1e3":       1000,
		"1E3":       1000,
		"2E":        2e18,
		" 64Mi ":    64 << 20,
	} {
		got, err := ParseQuantity(in)
		if err != nil || math.Abs(got-want) > want*1e-12 {
			t.Errorf("%q: %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Mi", "abc", "1.2.3Ki", "NaN", "Inf"} {
		if v, err := ParseQuantity(bad); err == nil {
			t.Errorf("%q parsed as %v", bad, v)
		}
	}
}
