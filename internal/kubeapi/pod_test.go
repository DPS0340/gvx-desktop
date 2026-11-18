package kubeapi

import (
	"testing"
	"time"
)

func TestSummarizeAsKubectlGetPods(t *testing.T) {
	at := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	yes := true
	running := ContainerState{Running: &Running{Started: at}}
	waiting := func(reason string) ContainerState { return ContainerState{Waiting: &Waiting{Reason: reason}} }
	ended := func(reason string, code, signal int) ContainerState {
		return ContainerState{Terminated: &Terminated{Reason: reason, ExitCode: code, Signal: signal, Finished: at}}
	}
	ctr := func(name string, ready bool, restarts int, state ContainerState) ContainerStatus {
		return ContainerStatus{Name: name, Ready: ready, Restarts: restarts, State: state}
	}
	spec := func(names ...string) PodSpec {
		var s PodSpec
		for _, n := range names {
			s.Containers = append(s.Containers, Container{Name: n})
		}
		return s
	}
	withInit := func(s PodSpec, names ...string) PodSpec {
		for _, n := range names {
			s.InitContainers = append(s.InitContainers, Container{Name: n})
		}
		return s
	}
	ready := []Condition{{Type: "Initialized", Status: "True"}, {Type: "Ready", Status: "True"}}
	deleted := &at

	for _, tc := range []struct {
		name     string
		pod      Pod
		status   string
		ready    string
		restarts int
		health   Health
	}{
		{"running", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Running", Conditions: ready,
			Containers: []ContainerStatus{ctr("a", true, 0, running)}}}, "Running", "1/1", 0, Healthy},
		{"running, not ready", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Running",
			Containers: []ContainerStatus{ctr("a", false, 0, running)}}}, "Running", "0/1", 0, Changing},
		{"unscheduled", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Pending",
			Conditions: []Condition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable"}}}}, "Pending", "0/1", 0, Changing},
		{"scheduling gated", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Pending",
			Conditions: []Condition{{Type: "PodScheduled", Status: "False", Reason: "SchedulingGated"}}}}, "SchedulingGated", "0/1", 0, Changing},
		{"creating", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Pending",
			Containers: []ContainerStatus{ctr("a", false, 0, waiting("ContainerCreating"))}}}, "ContainerCreating", "0/1", 0, Changing},
		{"crash loop", Pod{Spec: spec("a", "b"), Status: PodStatus{Phase: "Running",
			Containers: []ContainerStatus{ctr("a", false, 5, waiting("CrashLoopBackOff")), ctr("b", true, 0, running)}}}, "CrashLoopBackOff", "1/2", 5, Failing},
		{"image pull", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Pending",
			Containers: []ContainerStatus{ctr("a", false, 0, waiting("ImagePullBackOff"))}}}, "ImagePullBackOff", "0/1", 0, Failing},
		{"completed", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Succeeded",
			Containers: []ContainerStatus{ctr("a", false, 0, ended("Completed", 0, 0))}}}, "Completed", "0/1", 0, Done},
		{"error", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Failed",
			Containers: []ContainerStatus{ctr("a", false, 0, ended("Error", 1, 0))}}}, "Error", "0/1", 0, Failing},
		{"exit code", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Failed",
			Containers: []ContainerStatus{ctr("a", false, 0, ended("", 2, 0))}}}, "ExitCode:2", "0/1", 0, Failing},
		{"signal", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Failed",
			Containers: []ContainerStatus{ctr("a", false, 0, ended("", 137, 9))}}}, "Signal:9", "0/1", 0, Failing},
		// kubectl walks the containers from the last to the first, each
		// overwriting the reason: the first one's stands.
		{"first container wins", Pod{Spec: spec("a", "b"), Status: PodStatus{Phase: "Pending",
			Containers: []ContainerStatus{ctr("a", false, 0, waiting("ErrImagePull")), ctr("b", false, 0, waiting("ContainerCreating"))}}}, "ErrImagePull", "0/2", 0, Failing},
		{"completed beside one running, ready", Pod{Spec: spec("a", "b"), Status: PodStatus{Phase: "Running", Conditions: ready,
			Containers: []ContainerStatus{ctr("a", false, 0, ended("Completed", 0, 0)), ctr("b", true, 0, running)}}}, "Running", "1/2", 0, Changing},
		{"completed beside one running, not ready", Pod{Spec: spec("a", "b"), Status: PodStatus{Phase: "Running",
			Containers: []ContainerStatus{ctr("a", false, 0, ended("Completed", 0, 0)), ctr("b", true, 0, running)}}}, "NotReady", "1/2", 0, Changing},
		{"init running", Pod{Spec: withInit(spec("a"), "i1", "i2"), Status: PodStatus{Phase: "Pending",
			InitContainers: []ContainerStatus{ctr("i1", false, 0, running), ctr("i2", false, 0, waiting("PodInitializing"))},
			Containers:     []ContainerStatus{ctr("a", false, 0, waiting("PodInitializing"))}}}, "Init:0/2", "0/1", 0, Changing},
		{"second init", Pod{Spec: withInit(spec("a"), "i1", "i2"), Status: PodStatus{Phase: "Pending",
			InitContainers: []ContainerStatus{ctr("i1", false, 0, ended("Completed", 0, 0)), ctr("i2", false, 0, running)}}}, "Init:1/2", "0/1", 0, Changing},
		{"init crash loop", Pod{Spec: withInit(spec("a"), "i1"), Status: PodStatus{Phase: "Pending",
			InitContainers: []ContainerStatus{ctr("i1", false, 3, waiting("CrashLoopBackOff"))},
			Containers:     []ContainerStatus{ctr("a", false, 0, waiting("PodInitializing"))}}}, "Init:CrashLoopBackOff", "0/1", 3, Failing},
		{"init error", Pod{Spec: withInit(spec("a"), "i1"), Status: PodStatus{Phase: "Failed",
			InitContainers: []ContainerStatus{ctr("i1", false, 0, ended("Error", 1, 0))}}}, "Init:Error", "0/1", 0, Failing},
		{"init exit code", Pod{Spec: withInit(spec("a"), "i1"), Status: PodStatus{Phase: "Pending",
			InitContainers: []ContainerStatus{ctr("i1", false, 0, ended("", 3, 0))}}}, "Init:ExitCode:3", "0/1", 0, Failing},
		{"init signal", Pod{Spec: withInit(spec("a"), "i1"), Status: PodStatus{Phase: "Pending",
			InitContainers: []ContainerStatus{ctr("i1", false, 0, ended("", 143, 15))}}}, "Init:Signal:15", "0/1", 0, Failing},
		{"sidecar", Pod{Spec: PodSpec{Containers: []Container{{Name: "a"}}, InitContainers: []Container{{Name: "proxy", RestartPolicy: "Always"}}},
			Status: PodStatus{Phase: "Running", Conditions: ready,
				InitContainers: []ContainerStatus{{Name: "proxy", Ready: true, Started: &yes, Restarts: 2, State: running}},
				Containers:     []ContainerStatus{ctr("a", true, 1, running)}}}, "Running", "2/2", 3, Healthy},
		{"terminating", Pod{Metadata: Meta{Deleted: deleted}, Spec: spec("a"), Status: PodStatus{Phase: "Running", Conditions: ready,
			Containers: []ContainerStatus{ctr("a", true, 0, running)}}}, "Terminating", "1/1", 0, Changing},
		{"deleted after it ended", Pod{Metadata: Meta{Deleted: deleted}, Spec: spec("a"), Status: PodStatus{Phase: "Succeeded",
			Containers: []ContainerStatus{ctr("a", false, 0, ended("Completed", 0, 0))}}}, "Completed", "0/1", 0, Done},
		{"node lost", Pod{Metadata: Meta{Deleted: deleted}, Spec: spec("a"), Status: PodStatus{Phase: "Running", Reason: "NodeLost",
			Containers: []ContainerStatus{ctr("a", true, 0, running)}}}, "Unknown", "1/1", 0, Unknown},
		{"evicted", Pod{Spec: spec("a"), Status: PodStatus{Phase: "Failed", Reason: "Evicted",
			Message: "The node was low on resource: memory."}}, "Evicted", "0/1", 0, Failing},
	} {
		s := tc.pod.Summarize()
		if s.Status != tc.status || s.ReadyText() != tc.ready || s.Restarts != tc.restarts || s.Health() != tc.health {
			t.Errorf("%s: %s %s %d health %d, want %s %s %d health %d", tc.name, s.Status, s.ReadyText(), s.Restarts, s.Health(),
				tc.status, tc.ready, tc.restarts, tc.health)
		}
	}
}

func TestLastRestart(t *testing.T) {
	early, late := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	last := func(at time.Time) ContainerState {
		return ContainerState{Terminated: &Terminated{Reason: "Error", ExitCode: 1, Finished: at}}
	}
	p := Pod{Spec: PodSpec{Containers: []Container{{Name: "a"}, {Name: "b"}}}, Status: PodStatus{Phase: "Running", Containers: []ContainerStatus{
		{Name: "a", Restarts: 1, Last: last(late), State: ContainerState{Waiting: &Waiting{Reason: "CrashLoopBackOff", Message: "back-off 10s restarting failed container=a"}}},
		{Name: "b", Restarts: 2, Last: last(early), Ready: true, State: ContainerState{Running: &Running{}}},
	}}}
	s := p.Summarize()
	if s.Restarts != 3 || !s.LastRestart.Equal(late) {
		t.Errorf("%d restarts, the last at %v", s.Restarts, s.LastRestart)
	}
	if m := p.Message(); m != "a: back-off 10s restarting failed container=a" {
		t.Errorf("message %q", m)
	}
	if st, _ := p.ContainerStatusOf("a"); st.StateText() != "Waiting: CrashLoopBackOff" {
		t.Errorf("state %q", st.StateText())
	}
	if st, _ := p.ContainerStatusOf("b"); st.StateText() != "Running" {
		t.Errorf("state %q", st.StateText())
	}
}

func TestWorkload(t *testing.T) {
	rs := OwnerRef{Kind: "ReplicaSet", Name: "web-7d4b9c8f6d", Controller: true}
	for _, tc := range []struct {
		name   string
		meta   Meta
		kind   string
		owner  string
		hasOne bool
	}{
		{"deployment", Meta{Labels: map[string]string{"pod-template-hash": "7d4b9c8f6d"}, Owners: []OwnerRef{rs}}, "Deployment", "web", true},
		{"replica set alone", Meta{Owners: []OwnerRef{rs}}, "ReplicaSet", "web-7d4b9c8f6d", true},
		{"other hash", Meta{Labels: map[string]string{"pod-template-hash": "abc"}, Owners: []OwnerRef{rs}}, "ReplicaSet", "web-7d4b9c8f6d", true},
		{"daemon set", Meta{Owners: []OwnerRef{{Kind: "DaemonSet", Name: "svclb", Controller: true}}}, "DaemonSet", "svclb", true},
		{"job", Meta{Owners: []OwnerRef{{Kind: "Job", Name: "migrate", Controller: true}}}, "Job", "migrate", true},
		{"none", Meta{}, "", "", false},
	} {
		w, ok := Pod{Metadata: tc.meta}.Workload()
		if ok != tc.hasOne || w.Kind != tc.kind || w.Name != tc.owner {
			t.Errorf("%s: %+v %v", tc.name, w, ok)
		}
	}
}
