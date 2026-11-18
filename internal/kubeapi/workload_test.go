package kubeapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// request is what the test server was asked.
type request struct {
	method, uri, contentType, body string
}

// recorder is a test server's handler that notes each request and answers
// with what answer gives for it.
func recorder(calls *[]request, answer func(r *http.Request) (int, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*calls = append(*calls, request{r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), string(b)})
		code, body := answer(r)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

const (
	deploymentsJSON = `{"kind":"DeploymentList","apiVersion":"apps/v1","items":[
{"metadata":{"name":"web","namespace":"default","uid":"d1","generation":4,"creationTimestamp":"2026-11-16T04:00:00Z","labels":{"app":"web"}},
 "spec":{"replicas":3,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"nginx:alpine","ports":[{"containerPort":80}]},{"name":"exporter","image":"nginx/nginx-prometheus-exporter:1.4"}]}}},
 "status":{"observedGeneration":4,"replicas":3,"updatedReplicas":3,"readyReplicas":2,"availableReplicas":2}},
{"metadata":{"name":"idle","namespace":"default","generation":2,"creationTimestamp":"2026-11-16T04:00:00Z"},
 "spec":{"replicas":0,"template":{"spec":{"containers":[{"name":"idle","image":"busybox"}]}}},"status":{"observedGeneration":2}},
{"metadata":{"name":"fresh","namespace":"default","generation":1,"creationTimestamp":"2026-11-16T04:00:00Z"},
 "spec":{"template":{"spec":{"containers":[{"name":"a","image":"a:1"}]}}},"status":{}}]}`
	statefulSetsJSON = `{"items":[{"metadata":{"name":"redis","namespace":"shop","generation":1},
 "spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"redis","image":"redis:7-alpine"}]}}},
 "status":{"observedGeneration":1,"replicas":1,"readyReplicas":1,"currentReplicas":1,"updatedReplicas":1,"availableReplicas":1}}]}`
	daemonSetsJSON = `{"items":[{"metadata":{"name":"svclb","namespace":"kube-system","generation":1},
 "spec":{"template":{"spec":{"containers":[{"name":"lb","image":"rancher/klipper-lb:v0.4.9"}]}}},
 "status":{"observedGeneration":1,"desiredNumberScheduled":1,"currentNumberScheduled":1,"numberReady":1,"updatedNumberScheduled":1,"numberAvailable":1}}]}`
	jobsJSON = `{"items":[
{"metadata":{"name":"migrate-db","namespace":"default","generation":1},"spec":{"completions":1,"parallelism":1,"template":{"spec":{"containers":[{"name":"migrate","image":"app:dev"}]}}},
 "status":{"succeeded":1,"conditions":[{"type":"SuccessCriteriaMet","status":"True"},{"type":"Complete","status":"True"}]}},
{"metadata":{"name":"queue","namespace":"default"},"spec":{"parallelism":3,"template":{"spec":{"containers":[{"name":"w","image":"w"}]}}},"status":{"active":3}},
{"metadata":{"name":"broken","namespace":"default"},"spec":{"completions":2,"template":{"spec":{"containers":[{"name":"b","image":"b"}]}}},
 "status":{"failed":6,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}]}`
)

func TestWorkloads(t *testing.T) {
	dns, ips := k3sNames()
	var calls []request
	k := newCluster(t, recorder(&calls, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case "/apis/apps/v1/deployments":
			return 200, deploymentsJSON
		case "/apis/apps/v1/statefulsets":
			return 200, statefulSetsJSON
		case "/apis/apps/v1/daemonsets":
			return 200, daemonSetsJSON
		case "/apis/batch/v1/jobs":
			return 200, jobsJSON
		}
		return 500, ""
	}), dns, ips)
	list, err := connect(t, k.entry).Workloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range list {
		got = append(got, w.Key()+" "+w.ReadyText()+" "+strconv.Itoa(w.UpToDate)+" "+strings.Join(w.Images, ",")+" "+healthName(w.Health()))
	}
	want := []string{
		"Deployment/default/web 2/3 3 nginx:alpine,nginx/nginx-prometheus-exporter:1.4 changing",
		"Deployment/default/idle 0/0 0 busybox unknown",
		"Deployment/default/fresh 0/1 0 a:1 changing", // no replicas given: one
		"StatefulSet/shop/redis 1/1 1 redis:7-alpine healthy",
		"DaemonSet/kube-system/svclb 1/1 1 rancher/klipper-lb:v0.4.9 healthy",
		"Job/default/migrate-db 1/1 0 app:dev done",
		"Job/default/queue 0/3 0 w changing",
		"Job/default/broken 0/2 0 b failing",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("workloads\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	web := list[0]
	if !web.Metadata.Created.Equal(time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)) || web.Available != 2 || web.Rolling || !web.Scalable() || !web.Restartable() {
		t.Errorf("web %+v", web)
	}
	if list[2].Rolling != true { // its controller has yet to see generation 1
		t.Errorf("fresh %+v", list[2])
	}
	if j := list[5]; j.Scalable() || j.Restartable() || j.JobState != "Complete" {
		t.Errorf("job %+v", j)
	}
	if list[7].Failed != 6 || list[7].JobState != "Failed" {
		t.Errorf("broken %+v", list[7])
	}

	// Deleted, a workload is on its way out whatever its counts.
	gone := list[3]
	now := time.Now()
	gone.Metadata.Deleted = &now
	if gone.Health() != Changing {
		t.Errorf("a deleted workload: %v", gone.Health())
	}
}

func healthName(h Health) string {
	return [...]string{"healthy", "changing", "done", "failing", "unknown"}[h]
}

func TestWorkloadOwnsItsPods(t *testing.T) {
	pod := func(ns, kind, owner, hash string) Pod {
		p := Pod{Metadata: Meta{Namespace: ns, Name: "p", Labels: map[string]string{}}}
		if kind != "" {
			p.Metadata.Owners = []OwnerRef{{Kind: kind, Name: owner, Controller: true}}
		}
		if hash != "" {
			p.Metadata.Labels["pod-template-hash"] = hash
		}
		return p
	}
	web := Workload{Kind: KindDeployment, Metadata: Meta{Namespace: "default", Name: "web"}}
	redis := Workload{Kind: KindStatefulSet, Metadata: Meta{Namespace: "shop", Name: "redis"}}
	job := Workload{Kind: KindJob, Metadata: Meta{Namespace: "default", Name: "migrate-db"}}
	for _, c := range []struct {
		w    Workload
		p    Pod
		owns bool
	}{
		{web, pod("default", "ReplicaSet", "web-7d4b9c8f6d", "7d4b9c8f6d"), true},
		{web, pod("other", "ReplicaSet", "web-7d4b9c8f6d", "7d4b9c8f6d"), false},       // another namespace
		{web, pod("default", "ReplicaSet", "web-api-7d4b9c8f6d", "7d4b9c8f6d"), false}, // Deployment web-api
		{web, pod("default", "ReplicaSet", "web", ""), false},                          // a ReplicaSet of its own
		{redis, pod("shop", "StatefulSet", "redis", ""), true},                         // owned directly
		{redis, pod("shop", "Deployment", "redis", ""), false},                         // another kind of the name
		{job, pod("default", "Job", "migrate-db", ""), true},                           //
		{job, pod("default", "", "", ""), false},                                       // nothing owns it
		{Workload{Kind: KindDaemonSet, Metadata: Meta{Namespace: "kube-system", Name: "svclb"}}, pod("kube-system", "DaemonSet", "svclb", ""), true},
	} {
		if got := c.w.Owns(c.p); got != c.owns {
			t.Errorf("%s owns %+v: %v", c.w.Key(), c.p.Metadata, got)
		}
	}
}

func TestScaleRestartDelete(t *testing.T) {
	dns, ips := k3sNames()
	var calls []request
	k := newCluster(t, recorder(&calls, func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.Path, "/gone") {
			return 404, `{"kind":"Status","status":"Failure","message":"deployments.apps \"gone\" not found","reason":"NotFound","code":404}`
		}
		return 200, `{}`
	}), dns, ips)
	c := connect(t, k.entry)
	ctx := context.Background()
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

	if err := c.Scale(ctx, KindDeployment, "default", "web", 3); err != nil {
		t.Error(err)
	}
	if err := c.Scale(ctx, KindStatefulSet, "shop", "redis", 0); err != nil {
		t.Error(err)
	}
	if err := c.Restart(ctx, KindDeployment, "default", "web", at); err != nil {
		t.Error(err)
	}
	if err := c.Restart(ctx, KindDaemonSet, "kube-system", "svclb", at.In(time.FixedZone("KST", 9*3600))); err != nil {
		t.Error(err)
	}
	if err := c.DeleteWorkload(ctx, KindDeployment, "default", "web"); err != nil {
		t.Error(err)
	}
	if err := c.DeleteWorkload(ctx, KindJob, "default", "migrate-db"); err != nil {
		t.Error(err)
	}
	err := c.DeleteWorkload(ctx, KindDeployment, "default", "gone")
	var api *APIError
	if !errors.As(err, &api) || api.Reason != "NotFound" {
		t.Errorf("deleting one that is gone: %v", err)
	}
	// Refused before any request is made.
	for _, err := range []error{
		c.Scale(ctx, KindDaemonSet, "kube-system", "svclb", 2),
		c.Scale(ctx, KindJob, "default", "migrate-db", 2),
		c.Scale(ctx, KindDeployment, "default", "web", -1),
		c.Restart(ctx, KindJob, "default", "migrate-db", at),
		c.DeleteWorkload(ctx, "ReplicaSet", "default", "web-1"),
	} {
		if err == nil {
			t.Error("no error")
		}
	}

	const (
		merge = "application/merge-patch+json"
		del   = `{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Foreground"}`
	)
	want := []request{
		{"PATCH", "/apis/apps/v1/namespaces/default/deployments/web/scale", merge, `{"spec":{"replicas":3}}`},
		{"PATCH", "/apis/apps/v1/namespaces/shop/statefulsets/redis/scale", merge, `{"spec":{"replicas":0}}`},
		{"PATCH", "/apis/apps/v1/namespaces/default/deployments/web", merge, `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"2026-11-19T10:00:00Z"}}}}}`},
		{"PATCH", "/apis/apps/v1/namespaces/kube-system/daemonsets/svclb", merge, `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"2026-11-19T19:00:00+09:00"}}}}}`},
		{"DELETE", "/apis/apps/v1/namespaces/default/deployments/web", "application/json", del},
		{"DELETE", "/apis/batch/v1/namespaces/default/jobs/migrate-db", "application/json", del},
		{"DELETE", "/apis/apps/v1/namespaces/default/deployments/gone", "application/json", del},
	}
	if len(calls) != len(want) {
		t.Fatalf("%d requests: %+v", len(calls), calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("request %d\n%+v\nwant\n%+v", i, calls[i], want[i])
		}
	}
}
