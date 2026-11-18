package kubeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"time"
)

// The kinds of workload the app lists: what makes pods and keeps them.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
	KindJob         = "Job"
)

// Workload is a Deployment, StatefulSet, DaemonSet or Job, as far as the
// app shows one: how many pods it wants and how many are ready, as kubectl
// get counts them.
type Workload struct {
	Kind     string
	Metadata Meta
	// Desired is the pods it wants: its replicas, the nodes a DaemonSet
	// runs on, or the completions a Job needs.
	Desired int
	// Ready counts its ready pods, or for a Job those that succeeded.
	Ready int
	// UpToDate counts the pods made from its current template, and
	// Available those ready long enough to count; a Job has neither.
	UpToDate, Available int
	// Active and Failed count a Job's running and failed pods, and
	// JobState is its Complete or Failed condition, or "" while it runs.
	Active, Failed int
	JobState       string
	Images         []string
	// Rolling says the controller has yet to act on the latest change of
	// its spec, or its pods have yet to catch up with it.
	Rolling bool
}

// Equal makes Workload comparable for signals.
func (w Workload) Equal(o Workload) bool { return reflect.DeepEqual(w, o) }

// Name is the workload's name.
func (w Workload) Name() string { return w.Metadata.Name }

// Namespace is the workload's namespace.
func (w Workload) Namespace() string { return w.Metadata.Namespace }

// Key is kind/namespace/name, which names a workload in the cluster.
func (w Workload) Key() string { return w.Kind + "/" + w.Metadata.Namespace + "/" + w.Metadata.Name }

// ReadyText is kubectl get's READY column, or a Job's COMPLETIONS.
func (w Workload) ReadyText() string { return fmt.Sprintf("%d/%d", w.Ready, w.Desired) }

// Scalable reports whether the workload has a scale subresource the app
// sets: a Deployment's or a StatefulSet's.
func (w Workload) Scalable() bool { return w.Kind == KindDeployment || w.Kind == KindStatefulSet }

// Restartable reports whether a rollout restart applies: to what keeps its
// pods running from a template, not to a Job, whose pods run to an end.
func (w Workload) Restartable() bool { return w.Kind != KindJob }

// Owns reports whether p is one of the workload's pods.
func (w Workload) Owns(p Pod) bool {
	o, ok := p.Workload()
	return ok && p.Namespace() == w.Namespace() && o.Kind == w.Kind && o.Name == w.Name()
}

// Health is what the workload's counts mean: done or failed for a Job,
// healthy when all its pods are ready and current, changing on its way
// there or while it is deleted, and Unknown for one scaled to nothing.
func (w Workload) Health() Health {
	switch {
	case w.Metadata.Deleted != nil:
		return Changing
	case w.Kind == KindJob && w.JobState == "Complete":
		return Done
	case w.Kind == KindJob && w.JobState == "Failed":
		return Failing
	case w.Kind == KindJob:
		return Changing
	case w.Desired == 0:
		return Unknown
	case w.Ready >= w.Desired && w.UpToDate >= w.Desired && !w.Rolling:
		return Healthy
	}
	return Changing
}

// template is the part of a pod template the app reads: its images.
type template struct {
	Spec struct {
		Containers []Container `json:"containers"`
	} `json:"spec"`
}

func (t template) images() []string {
	out := make([]string, 0, len(t.Spec.Containers))
	for _, c := range t.Spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

// meta is Meta with the generation the controller is to catch up with.
type meta struct {
	Meta
	Generation int64 `json:"generation"`
}

type deployment struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Replicas *int     `json:"replicas"`
		Template template `json:"template"`
	} `json:"spec"`
	Status struct {
		Observed  int64 `json:"observedGeneration"`
		Replicas  int   `json:"replicas"`
		Ready     int   `json:"readyReplicas"`
		Updated   int   `json:"updatedReplicas"`
		Available int   `json:"availableReplicas"`
	} `json:"status"`
}

type statefulSet struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Replicas *int     `json:"replicas"`
		Template template `json:"template"`
	} `json:"spec"`
	Status struct {
		Observed  int64 `json:"observedGeneration"`
		Replicas  int   `json:"replicas"`
		Ready     int   `json:"readyReplicas"`
		Updated   int   `json:"updatedReplicas"`
		Available int   `json:"availableReplicas"`
	} `json:"status"`
}

type daemonSet struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Template template `json:"template"`
	} `json:"spec"`
	Status struct {
		Observed  int64 `json:"observedGeneration"`
		Desired   int   `json:"desiredNumberScheduled"`
		Ready     int   `json:"numberReady"`
		Updated   int   `json:"updatedNumberScheduled"`
		Available int   `json:"numberAvailable"`
	} `json:"status"`
}

type job struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Completions *int     `json:"completions"`
		Parallelism *int     `json:"parallelism"`
		Template    template `json:"template"`
	} `json:"spec"`
	Status struct {
		Active     int         `json:"active"`
		Succeeded  int         `json:"succeeded"`
		Failed     int         `json:"failed"`
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

// replicas is a spec's replicas, which the API server defaults to one.
func replicas(n *int) int {
	if n == nil {
		return 1
	}
	return *n
}

func (d deployment) workload() Workload {
	want := replicas(d.Spec.Replicas)
	s := d.Status
	return Workload{Kind: KindDeployment, Metadata: d.Metadata.Meta, Desired: want, Ready: s.Ready, UpToDate: s.Updated,
		Available: s.Available, Images: d.Spec.Template.images(),
		Rolling: s.Observed < d.Metadata.Generation || s.Replicas > s.Updated}
}

func (st statefulSet) workload() Workload {
	want := replicas(st.Spec.Replicas)
	s := st.Status
	return Workload{Kind: KindStatefulSet, Metadata: st.Metadata.Meta, Desired: want, Ready: s.Ready, UpToDate: s.Updated,
		Available: s.Available, Images: st.Spec.Template.images(),
		Rolling: s.Observed < st.Metadata.Generation || s.Replicas > s.Updated}
}

func (d daemonSet) workload() Workload {
	s := d.Status
	return Workload{Kind: KindDaemonSet, Metadata: d.Metadata.Meta, Desired: s.Desired, Ready: s.Ready, UpToDate: s.Updated,
		Available: s.Available, Images: d.Spec.Template.images(), Rolling: s.Observed < d.Metadata.Generation}
}

// workload is the Job as kubectl get jobs shows it: its COMPLETIONS, the
// pods that succeeded of those it needs, and its state.
func (j job) workload() Workload {
	s := j.Status
	want := 1
	switch {
	case j.Spec.Completions != nil:
		want = *j.Spec.Completions
	case j.Spec.Parallelism != nil && *j.Spec.Parallelism > 1:
		want = *j.Spec.Parallelism // a work queue: done when one succeeds and the rest end
	}
	w := Workload{Kind: KindJob, Metadata: j.Metadata.Meta, Desired: want, Ready: s.Succeeded, Active: s.Active,
		Failed: s.Failed, Images: j.Spec.Template.images()}
	for _, c := range s.Conditions {
		if (c.Type == "Complete" || c.Type == "Failed") && c.Status == "True" {
			w.JobState = c.Type
		}
	}
	return w
}

// list reads a list of every namespace into items.
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var l struct {
		Items []T `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, path, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Workloads lists the Deployments, StatefulSets, DaemonSets and Jobs of
// every namespace, in that order.
func (c *Client) Workloads(ctx context.Context) ([]Workload, error) {
	var out []Workload
	deps, err := list[deployment](ctx, c, "/apis/apps/v1/deployments")
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		out = append(out, d.workload())
	}
	sets, err := list[statefulSet](ctx, c, "/apis/apps/v1/statefulsets")
	if err != nil {
		return nil, err
	}
	for _, s := range sets {
		out = append(out, s.workload())
	}
	daemons, err := list[daemonSet](ctx, c, "/apis/apps/v1/daemonsets")
	if err != nil {
		return nil, err
	}
	for _, d := range daemons {
		out = append(out, d.workload())
	}
	jobs, err := list[job](ctx, c, "/apis/batch/v1/jobs")
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		out = append(out, j.workload())
	}
	return out, nil
}

// workloadPath is the path of a workload's resource.
func workloadPath(kind, namespace, name string) (string, error) {
	group, plural := "apis/apps/v1", ""
	switch kind {
	case KindDeployment:
		plural = "deployments"
	case KindStatefulSet:
		plural = "statefulsets"
	case KindDaemonSet:
		plural = "daemonsets"
	case KindJob:
		group, plural = "apis/batch/v1", "jobs"
	default:
		return "", fmt.Errorf("kubeapi: %q is not a workload kind", kind)
	}
	return "/" + group + "/namespaces/" + url.PathEscape(namespace) + "/" + plural + "/" + url.PathEscape(name), nil
}

// mergePatch is the content type of a JSON merge patch (RFC 7386), which
// any resource takes.
const mergePatch = "application/merge-patch+json"

// Scale sets how many replicas a Deployment or StatefulSet wants, through
// its scale subresource, as kubectl scale does.
func (c *Client) Scale(ctx context.Context, kind, namespace, name string, n int) error {
	if kind != KindDeployment && kind != KindStatefulSet {
		return fmt.Errorf("kubeapi: a %s is not scaled", kind)
	}
	if n < 0 {
		return fmt.Errorf("kubeapi: %d replicas", n)
	}
	path, err := workloadPath(kind, namespace, name)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"spec": map[string]int{"replicas": n}})
	return c.doBody(ctx, http.MethodPatch, path+"/scale", mergePatch, body, nil)
}

// RestartedAt is the annotation of a pod template kubectl rollout restart
// sets: a new value is a new template, which the controller rolls out.
const RestartedAt = "kubectl.kubernetes.io/restartedAt"

// Restart replaces the pods of a Deployment, StatefulSet or DaemonSet, as
// kubectl rollout restart does: it marks the pod template with the time,
// and the controller rolls out the changed template as it would any other.
func (c *Client) Restart(ctx context.Context, kind, namespace, name string, at time.Time) error {
	if kind == KindJob {
		return fmt.Errorf("kubeapi: a %s is not restarted", kind)
	}
	path, err := workloadPath(kind, namespace, name)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{RestartedAt: at.Format(time.RFC3339)},
	}}}})
	return c.doBody(ctx, http.MethodPatch, path, mergePatch, body, nil)
}

// DeleteWorkload deletes a workload with its pods, in the foreground: the
// workload stays, marked as being deleted, until the garbage collector has
// deleted what it made (a Deployment's ReplicaSets, and the pods), so
// listing it shows it going while its pods do. In the background, as
// kubectl deletes, it would vanish at once and leave its pods to go
// unseen; and a Job deleted through the API with no policy at all leaves
// its pods behind.
func (c *Client) DeleteWorkload(ctx context.Context, kind, namespace, name string) error {
	path, err := workloadPath(kind, namespace, name)
	if err != nil {
		return err
	}
	return c.doBody(ctx, http.MethodDelete, path, "application/json", []byte(foregroundDelete), nil)
}

// foregroundDelete is the DeleteOptions DeleteWorkload sends.
const foregroundDelete = `{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Foreground"}`
