package recipes

// The Kubernetes recipe's cluster grows by agents, one sandbox each, on an
// overlay network of the app's (docs/design/k3s-clusters.md §2): a node's
// place on it is its --node-ip.

import (
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// KubernetesAgent is the recipe of a sandbox that runs an agent of the
// Kubernetes recipe's cluster.
const KubernetesAgent = "k3s-agent"

// NodeSubnet is the network of the cluster's nodes: the server is
// NodeServerAddr, agent n is the n-th address after it.
var NodeSubnet = netip.MustParsePrefix("10.88.0.0/24")

// NodeServerAddr is the server's address on NodeSubnet.
var NodeServerAddr = netip.MustParseAddr("10.88.0.2")

// NodeIface is the overlay's link in a node, the one flannel uses.
const NodeIface = "gvx0"

// K3sEntrypoint is the k3s image's entrypoint, which a node's spec leaves
// to the image.
const K3sEntrypoint = "/bin/k3s"

// NodeTokenFile is where a k3s server keeps the token its agents join with.
const NodeTokenFile = "/var/lib/rancher/k3s/server/node-token"

// MaxAgents is how many agents NodeSubnet holds.
const MaxAgents = 250

// IsKubernetesNode reports whether spec runs a node of the recipe's
// cluster: its server or an agent.
func IsKubernetesNode(spec sandbox.Spec) bool {
	return spec.Recipe == Kubernetes || spec.Recipe == KubernetesAgent
}

// NodeAddr is spec's address on the nodes' network, when it is a node
// that joins it: its --node-ip, in NodeSubnet.
func NodeAddr(spec sandbox.Spec) (netip.Addr, bool) {
	if !IsKubernetesNode(spec) {
		return netip.Addr{}, false
	}
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return netip.Addr{}, false
	}
	v, ok := flagValue(args, "--node-ip")
	if !ok {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(v)
	if err != nil || !NodeSubnet.Contains(a) {
		return netip.Addr{}, false
	}
	return a, true
}

// AgentAddr is agent n's address (n from 1).
func AgentAddr(n int) netip.Addr {
	a := NodeServerAddr
	for range n {
		a = a.Next()
	}
	return a
}

// nodeFlags are the flags that put a node with address a on the network.
func nodeFlags(a netip.Addr) []string {
	return []string{"--node-ip=" + a.String(), "--flannel-iface=" + NodeIface}
}

// WithNodeNetwork returns the cluster's server on the nodes' network, which
// it needs before an agent can join: its command gets nodeFlags after the
// subcommand. It takes a restart. A server on it already is returned as it
// is.
func WithNodeNetwork(server sandbox.Spec) sandbox.Spec {
	if _, ok := NodeAddr(server); ok || server.Recipe != Kubernetes {
		return server
	}
	args, err := rungv.SplitArgs(server.Command)
	if err != nil {
		return server
	}
	at := 0
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		at = 1 // after "server"
	}
	out := server.Clone()
	out.Command = rungv.FormatArgs(slices.Insert(args, at, nodeFlags(NodeServerAddr)...))
	return out
}

// agentNumber is n of the agent named k3s-agent-n, or 0.
func agentNumber(spec sandbox.Spec) int {
	if spec.Recipe != KubernetesAgent {
		return 0
	}
	a, ok := NodeAddr(spec)
	if !ok {
		return 0
	}
	n := 0
	for b := NodeServerAddr; b.Less(a); b = b.Next() {
		n++
	}
	return n
}

// NextAgent is the lowest agent number no agent in specs has, or 0 when
// the network is full.
func NextAgent(specs []sandbox.Spec) int {
	used := map[int]bool{}
	for _, s := range specs {
		if n := agentNumber(s); n > 0 {
			used[n] = true
		}
	}
	for n := 1; n <= MaxAgents; n++ {
		if !used[n] {
			return n
		}
	}
	return 0
}

// Agents are the specs of the cluster's agents, in their order on the
// network.
func Agents(specs []sandbox.Spec) []sandbox.Spec {
	var out []sandbox.Spec
	for _, s := range specs {
		if agentNumber(s) > 0 {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b sandbox.Spec) int { return agentNumber(a) - agentNumber(b) })
	return out
}

// KubernetesAgentNode is agent n of server's cluster, joining with token:
// the server's image and resources, its own volume for /var/lib/rancher,
// and the folder the server writes its kubeconfig to at /output, for the
// registries file.
func KubernetesAgentNode(server sandbox.Spec, n int, token string) sandbox.Spec {
	name := fmt.Sprintf("k3s-agent-%d", n)
	args := append([]string{"agent"}, nodeFlags(AgentAddr(n))...)
	env := []string{
		"K3S_URL=https://" + NodeServerAddr.String() + ":" + strconv.Itoa(k3sAPIPort),
		"K3S_TOKEN=" + token,
	}
	for _, kv := range server.Env {
		if k, _, _ := strings.Cut(kv, "="); k == "IPTABLES_MODE" {
			env = append(env, kv)
		}
	}
	spec := sandbox.Spec{
		ID:       sandbox.NewID(),
		Name:     name,
		Hostname: name,
		Image:    server.Image,
		Env:      env,
		Mounts:   []sandbox.Mount{{Type: sandbox.MountVolume, Source: name, Target: "/var/lib/rancher"}},
		Memory:   server.Memory,
		CPUs:     server.CPUs,
		Backend:  server.Backend,
		Recipe:   KubernetesAgent,
		// The agent's last line as its node registers.
		ReadyPattern: "Annotations and labels have been set successfully on node",
		CreatedAt:    time.Now(),
	}
	for _, m := range server.Mounts {
		if m.Type == sandbox.MountBind && m.Target == path.Dir(registriesFile) {
			spec.Mounts = append(spec.Mounts, m)
			args = append(args, privateRegistry())
		}
	}
	spec.Command = rungv.FormatArgs(args)
	return spec
}
