package recipes

import (
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestWithNodeNetwork(t *testing.T) {
	server := KubernetesNode()
	if _, ok := NodeAddr(server); ok {
		t.Fatal("a new cluster's server is on the nodes' network")
	}
	on := WithNodeNetwork(server)
	a, ok := NodeAddr(on)
	if !ok || a != NodeServerAddr {
		t.Fatalf("NodeAddr = %v, %v", a, ok)
	}
	args, _ := rungv.SplitArgs(on.Command)
	if args[0] != "server" || args[1] != "--node-ip=10.88.0.2" || args[2] != "--flannel-iface=gvx0" {
		t.Errorf("command %q", on.Command)
	}
	if !strings.Contains(on.Command, privateRegistry()) || ServiceLBOf(on) != ServiceLBOf(server) {
		t.Errorf("the other flags changed: %q", on.Command)
	}
	if again := WithNodeNetwork(on); again.Command != on.Command {
		t.Errorf("twice: %q", again.Command)
	}
	if server.Command == on.Command {
		t.Error("the server given was changed")
	}
	// A node address outside the subnet, or a sandbox of another recipe,
	// is not on the network.
	other := server.Clone()
	other.Command = "server --node-ip=192.168.1.5"
	if _, ok := NodeAddr(other); ok {
		t.Error("an address outside the subnet")
	}
	other = on.Clone()
	other.Recipe = Docker
	if _, ok := NodeAddr(other); ok {
		t.Error("a sandbox of another recipe")
	}
}

func TestKubernetesAgentNode(t *testing.T) {
	server := WithNodeNetwork(KubernetesNode())
	server.Memory, server.CPUs = "4g", "2"
	agent := KubernetesAgentNode(server, 3, "K10abc::server:secret")
	if agent.Name != "k3s-agent-3" || agent.Hostname != agent.Name || agent.Recipe != KubernetesAgent || agent.Image != server.Image {
		t.Errorf("agent %+v", agent)
	}
	if a, ok := NodeAddr(agent); !ok || a.String() != "10.88.0.5" {
		t.Errorf("NodeAddr = %v, %v", a, ok)
	}
	for _, want := range []string{"K3S_URL=https://10.88.0.2:6443", "K3S_TOKEN=K10abc::server:secret", "IPTABLES_MODE=legacy"} {
		if !slices.Contains(agent.Env, want) {
			t.Errorf("env lacks %s: %q", want, agent.Env)
		}
	}
	args, _ := rungv.SplitArgs(agent.Command)
	if args[0] != "agent" || !slices.Contains(args, "--flannel-iface=gvx0") || !slices.Contains(args, privateRegistry()) {
		t.Errorf("command %q", agent.Command)
	}
	if agent.Memory != "4g" || agent.CPUs != "2" || len(agent.Ports) != 0 {
		t.Errorf("resources and ports: %+v", agent)
	}
	if v := agent.Volumes(); !slices.Equal(v, []string{"k3s-agent-3"}) {
		t.Errorf("volumes %q", v)
	}
	if err := agent.Validate(); err != nil {
		t.Error(err)
	}
}

func TestNextAgentAndAgents(t *testing.T) {
	server := WithNodeNetwork(KubernetesNode())
	specs := []sandbox.Spec{server, DockerEngine()}
	if n := NextAgent(specs); n != 1 {
		t.Fatalf("NextAgent = %d", n)
	}
	specs = append(specs, KubernetesAgentNode(server, 2, "t"), KubernetesAgentNode(server, 1, "t"), KubernetesAgentNode(server, 4, "t"))
	if n := NextAgent(specs); n != 3 {
		t.Errorf("NextAgent = %d, want the gap", n)
	}
	var names []string
	for _, a := range Agents(specs) {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"k3s-agent-1", "k3s-agent-2", "k3s-agent-4"}) {
		t.Errorf("Agents = %q", names)
	}
	if a := AgentAddr(MaxAgents); a.String() != "10.88.0.252" {
		t.Errorf("the last agent's address %v", a)
	}
}
