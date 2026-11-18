package kubeapi

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Pod is the part of a v1 Pod the app shows.
type Pod struct {
	Metadata Meta      `json:"metadata"`
	Spec     PodSpec   `json:"spec"`
	Status   PodStatus `json:"status"`
}

// Meta is the part of an object's metadata the app shows.
type Meta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	UID       string            `json:"uid"`
	Created   time.Time         `json:"creationTimestamp"`
	Deleted   *time.Time        `json:"deletionTimestamp,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Owners    []OwnerRef        `json:"ownerReferences,omitempty"`
}

// OwnerRef names an object a pod belongs to.
type OwnerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller bool   `json:"controller,omitempty"`
}

// PodSpec is the part of a pod's spec the app shows.
type PodSpec struct {
	Node           string      `json:"nodeName,omitempty"`
	Containers     []Container `json:"containers"`
	InitContainers []Container `json:"initContainers,omitempty"`
}

// Container is a container of a pod's spec.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// RestartPolicy Always makes an init container a sidecar, which runs
	// beside the others.
	RestartPolicy string `json:"restartPolicy,omitempty"`
}

// PodStatus is the part of a pod's status the app shows.
type PodStatus struct {
	Phase          string            `json:"phase"`
	Reason         string            `json:"reason,omitempty"`
	Message        string            `json:"message,omitempty"`
	PodIP          string            `json:"podIP,omitempty"`
	Conditions     []Condition       `json:"conditions,omitempty"`
	Containers     []ContainerStatus `json:"containerStatuses,omitempty"`
	InitContainers []ContainerStatus `json:"initContainerStatuses,omitempty"`
}

// Condition is a condition of a pod, such as Ready.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerStatus is the state of a container of a pod.
type ContainerStatus struct {
	Name     string         `json:"name"`
	Image    string         `json:"image"`
	Ready    bool           `json:"ready"`
	Started  *bool          `json:"started,omitempty"`
	Restarts int            `json:"restartCount"`
	State    ContainerState `json:"state"`
	Last     ContainerState `json:"lastState"`
}

// ContainerState is one of waiting, running or terminated.
type ContainerState struct {
	Waiting    *Waiting    `json:"waiting,omitempty"`
	Running    *Running    `json:"running,omitempty"`
	Terminated *Terminated `json:"terminated,omitempty"`
}

// Waiting is a container that has not started, and why.
type Waiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Running is a container that runs.
type Running struct {
	Started time.Time `json:"startedAt"`
}

// Terminated is a container that ended.
type Terminated struct {
	ExitCode int       `json:"exitCode"`
	Signal   int       `json:"signal,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Message  string    `json:"message,omitempty"`
	Started  time.Time `json:"startedAt"`
	Finished time.Time `json:"finishedAt"`
}

// Equal makes Pod comparable for signals.
func (p Pod) Equal(o Pod) bool { return reflect.DeepEqual(p, o) }

// Name is the pod's name.
func (p Pod) Name() string { return p.Metadata.Name }

// Namespace is the pod's namespace.
func (p Pod) Namespace() string { return p.Metadata.Namespace }

// Key is namespace/name, which names a pod in the cluster.
func (p Pod) Key() string { return p.Metadata.Namespace + "/" + p.Metadata.Name }

// Owner is the object that controls the pod, if any.
func (p Pod) Owner() (OwnerRef, bool) {
	for _, o := range p.Metadata.Owners {
		if o.Controller {
			return o, true
		}
	}
	if len(p.Metadata.Owners) > 0 {
		return p.Metadata.Owners[0], true
	}
	return OwnerRef{}, false
}

// Workload is what makes the pod and replaces it when it goes: its owner,
// or for a ReplicaSet a Deployment made, the Deployment. A Deployment
// names its ReplicaSets after itself and the pod-template-hash label of
// their pods, which is how one is told from a ReplicaSet made alone.
func (p Pod) Workload() (OwnerRef, bool) {
	o, ok := p.Owner()
	if !ok {
		return o, false
	}
	if hash := p.Metadata.Labels["pod-template-hash"]; o.Kind == "ReplicaSet" && hash != "" {
		if name, found := strings.CutSuffix(o.Name, "-"+hash); found && name != "" {
			return OwnerRef{Kind: "Deployment", Name: name, Controller: true}, true
		}
	}
	return o, true
}

// ContainerNames are the names of the pod's containers, init containers
// after the others: the ones logs can be read of.
func (p Pod) ContainerNames() []string {
	out := make([]string, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers))
	for _, c := range p.Spec.Containers {
		out = append(out, c.Name)
	}
	for _, c := range p.Spec.InitContainers {
		out = append(out, c.Name)
	}
	return out
}

// ContainerStatusOf is the state of the container named name, init
// containers included.
func (p Pod) ContainerStatusOf(name string) (ContainerStatus, bool) {
	for _, s := range p.Status.Containers {
		if s.Name == name {
			return s, true
		}
	}
	for _, s := range p.Status.InitContainers {
		if s.Name == name {
			return s, true
		}
	}
	return ContainerStatus{}, false
}

// IsInit reports whether the container named name is an init container.
func (p Pod) IsInit(name string) bool {
	for _, c := range p.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Summary is a pod as kubectl get pods shows it: the STATUS, READY and
// RESTARTS columns, and when a container last restarted.
type Summary struct {
	Status      string
	Ready       int
	Total       int
	Restarts    int
	LastRestart time.Time // zero when none did, or the time is not known
}

// ReadyText is the READY column: ready containers of all of them.
func (s Summary) ReadyText() string { return fmt.Sprintf("%d/%d", s.Ready, s.Total) }

// Summarize works out what kubectl get pods shows of the pod, by the
// rules of printPod in kubernetes/pkg/printers/internalversion/printers.go
// (v1.31): the phase or the pod's reason, overridden by the first init
// container that has not finished, then by the containers' waiting or
// terminated reasons, Running again if a container still runs and the pod
// is ready, and Terminating while a pod not yet ended is being deleted.
func (p Pod) Summarize() Summary {
	st := p.Status
	s := Summary{Total: len(p.Spec.Containers)}
	reason := st.Phase
	if st.Reason != "" {
		reason = st.Reason
	}
	for _, c := range st.Conditions {
		if c.Type == "PodScheduled" && c.Reason == "SchedulingGated" {
			reason = "SchedulingGated"
		}
	}

	sidecar := map[string]bool{}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy == "Always" {
			sidecar[c.Name] = true
			s.Total++
		}
	}

	var lastRestart, lastSidecarRestart time.Time
	later := func(t *time.Time, last ContainerState) {
		if last.Terminated != nil && last.Terminated.Finished.After(*t) {
			*t = last.Terminated.Finished
		}
	}
	restarts, sidecarRestarts := 0, 0
	initializing := false
	for i, c := range st.InitContainers {
		restarts += c.Restarts
		later(&lastRestart, c.Last)
		if sidecar[c.Name] {
			sidecarRestarts += c.Restarts
			later(&lastSidecarRestart, c.Last)
		}
		t, w := c.State.Terminated, c.State.Waiting
		switch {
		case t != nil && t.ExitCode == 0:
			continue
		case sidecar[c.Name] && c.Started != nil && *c.Started:
			if c.Ready {
				s.Ready++
			}
			continue
		case t != nil:
			switch {
			case t.Reason != "":
				reason = "Init:" + t.Reason
			case t.Signal != 0:
				reason = fmt.Sprintf("Init:Signal:%d", t.Signal)
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", t.ExitCode)
			}
		case w != nil && w.Reason != "" && w.Reason != "PodInitializing":
			reason = "Init:" + w.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}

	if !initializing || condition(st.Conditions, "Initialized") {
		restarts, lastRestart = sidecarRestarts, lastSidecarRestart
		running := false
		for i := len(st.Containers) - 1; i >= 0; i-- {
			c := st.Containers[i]
			restarts += c.Restarts
			later(&lastRestart, c.Last)
			w, t := c.State.Waiting, c.State.Terminated
			switch {
			case w != nil && w.Reason != "":
				reason = w.Reason
			case t != nil && t.Reason != "":
				reason = t.Reason
			case t != nil && t.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", t.Signal)
			case t != nil:
				reason = fmt.Sprintf("ExitCode:%d", t.ExitCode)
			case c.Ready && c.State.Running != nil:
				running = true
				s.Ready++
			}
		}
		if reason == "Completed" && running {
			reason = "NotReady"
			if condition(st.Conditions, "Ready") {
				reason = "Running"
			}
		}
	}

	switch {
	case p.Metadata.Deleted != nil && st.Reason == "NodeLost":
		reason = "Unknown"
	case p.Metadata.Deleted != nil && st.Phase != "Succeeded" && st.Phase != "Failed":
		reason = "Terminating"
	}
	s.Status, s.Restarts, s.LastRestart = reason, restarts, lastRestart
	return s
}

// condition reports whether the pod's condition of type is True.
func condition(cs []Condition, typ string) bool {
	for _, c := range cs {
		if c.Type == typ {
			return c.Status == "True"
		}
	}
	return false
}

// Health is what a pod's state means to someone reading it.
type Health int

const (
	Healthy  Health = iota // running, all its containers ready
	Changing               // on its way somewhere: pending, starting, terminating
	Done                   // ran to completion
	Failing                // crashed, cannot pull, failed
	Unknown                // the node does not say
)

// Health classes the status Summarize worked out.
func (s Summary) Health() Health {
	switch st := s.Status; {
	case st == "Running":
		if s.Ready < s.Total {
			return Changing
		}
		return Healthy
	case st == "Completed" || st == "Succeeded":
		return Done
	case st == "Unknown":
		return Unknown
	case st == "Pending", st == "ContainerCreating", st == "PodInitializing", st == "Terminating",
		st == "SchedulingGated", st == "NotReady", strings.HasPrefix(st, "Init:") && isInitProgress(st):
		return Changing
	}
	return Failing
}

// isInitProgress reports whether an Init: status counts init containers
// done, as Init:1/2 does, rather than telling of one that failed.
func isInitProgress(st string) bool {
	rest := strings.TrimPrefix(st, "Init:")
	n, total, ok := strings.Cut(rest, "/")
	return ok && n != "" && total != "" && strings.Trim(n+total, "0123456789") == "" ||
		rest == "PodInitializing" || rest == "ContainerCreating"
}

// Message is what the pod or its containers say of a state that is not
// plain running: a waiting or terminated container's message, or the
// pod's, such as why it is not scheduled.
func (p Pod) Message() string {
	for _, list := range [][]ContainerStatus{p.Status.InitContainers, p.Status.Containers} {
		for _, c := range list {
			if w := c.State.Waiting; w != nil && w.Message != "" {
				return c.Name + ": " + w.Message
			}
			if t := c.State.Terminated; t != nil && t.Message != "" && t.ExitCode != 0 {
				return c.Name + ": " + t.Message
			}
		}
	}
	if p.Status.Message != "" {
		return p.Status.Message
	}
	for _, c := range p.Status.Conditions {
		if c.Status != "True" && c.Message != "" {
			return c.Message
		}
	}
	return ""
}

// StateText is a container's state in kubectl describe's words: Running,
// Waiting (its reason), or Terminated (its reason and exit code).
func (c ContainerStatus) StateText() string {
	switch s := c.State; {
	case s.Running != nil:
		return "Running"
	case s.Waiting != nil:
		if s.Waiting.Reason != "" {
			return "Waiting: " + s.Waiting.Reason
		}
		return "Waiting"
	case s.Terminated != nil:
		r := s.Terminated.Reason
		if r == "" {
			r = "Terminated"
		}
		return fmt.Sprintf("%s (exit %d)", r, s.Terminated.ExitCode)
	}
	return "Unknown"
}
