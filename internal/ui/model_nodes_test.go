package ui

import (
	"testing"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func kubeNode(name string, ready, server bool) kubeapi.Node {
	var n kubeapi.Node
	n.Metadata.Name = name
	if server {
		n.Metadata.Labels = map[string]string{controlPlaneLabel: "true"}
	}
	status := "False"
	if ready {
		status = "True"
	}
	n.Status.Conditions = []kubeapi.Condition{{Type: "Ready", Status: status}}
	n.Status.NodeInfo.KubeletVersion = "v1.31.4+k3s1"
	return n
}

func TestClusterNodes(t *testing.T) {
	server := recipes.WithNodeNetwork(recipes.KubernetesNode())
	a2 := recipes.KubernetesAgentNode(server, 2, "t")
	a1 := recipes.KubernetesAgentNode(server, 1, "t")
	running := sandbox.State{Status: sandbox.Running, Control: true}
	rows := []Row{
		{Spec: a2, State: running},
		{Spec: recipes.DockerEngine(), State: running},
		{Spec: server, State: running},
		{Spec: a1},
	}
	got := clusterNodes(rows, []kubeapi.Node{kubeNode("k3s-agent-2", false, false), kubeNode("gvx", true, true)})
	if len(got) != 3 {
		t.Fatalf("%d nodes, want the server and two agents", len(got))
	}
	if s := got[0]; !s.Server || s.Addr != "10.88.0.2" || !s.Registered || !s.Ready || s.Version != "v1.31.4+k3s1" {
		t.Errorf("server %+v", s)
	}
	if a := got[1]; a.Row.Spec.Name != "k3s-agent-1" || a.Server || a.Addr != "10.88.0.3" || a.Registered {
		t.Errorf("first agent %+v", a)
	}
	if a := got[2]; a.Row.Spec.Name != "k3s-agent-2" || !a.Registered || a.Ready {
		t.Errorf("second agent %+v", a)
	}

	// A single-node cluster, off the nodes' network.
	alone := clusterNodes([]Row{{Spec: recipes.KubernetesNode(), State: running}}, nil)
	if len(alone) != 1 || alone[0].Addr != "" || alone[0].Registered {
		t.Errorf("a single node %+v", alone)
	}
}

func TestAddNodeBlocked(t *testing.T) {
	server := recipes.KubernetesNode()
	for _, c := range []struct {
		name  string
		rows  []Row
		empty bool
	}{
		{"no cluster", nil, false},
		{"stopped", []Row{{Spec: server}}, false},
		{"paused", []Row{{Spec: server, State: sandbox.State{Status: sandbox.Running, Paused: true, Control: true}}}, false},
		{"no control", []Row{{Spec: server, State: sandbox.State{Status: sandbox.Running}}}, false},
		{"running", []Row{{Spec: server, State: sandbox.State{Status: sandbox.Running, Control: true}}}, true},
	} {
		if got := AddNodeBlocked(c.rows); (got == "") != c.empty {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}
