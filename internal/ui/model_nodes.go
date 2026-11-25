package ui

// The cluster's nodes: its server, the Kubernetes recipe's sandbox, and
// the agents that join it, one sandbox each, on the nodes' network
// (internal/nodenet, docs/design/k3s-clusters.md §2).

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/nodenet"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func nodesEqual(a, b []kubeapi.Node) bool { return reflect.DeepEqual(a, b) }

// ClusterNode is a node of the cluster as the Nodes card shows it: its
// sandbox, and what the API server says of it, once it registered.
type ClusterNode struct {
	Row    Row
	Server bool
	// Addr is its address on the nodes' network, "" while it is off it.
	Addr string
	// Registered says the API server lists it; Ready and Version are its.
	Registered, Ready bool
	Version           string
}

// Equal compares two nodes.
func (n ClusterNode) Equal(o ClusterNode) bool {
	return n.Row.Equal(o.Row) && n.Server == o.Server && n.Addr == o.Addr &&
		n.Registered == o.Registered && n.Ready == o.Ready && n.Version == o.Version
}

// controlPlaneLabel marks a server's node.
const controlPlaneLabel = "node-role.kubernetes.io/control-plane"

// clusterNodes lists the server and the agents of rows, in that order,
// with their nodes in kube. An agent's node has its sandbox's name; the
// server's is the one with the control plane.
func clusterNodes(rows []Row, kube []kubeapi.Node) []ClusterNode {
	var specs []sandbox.Spec
	byID := map[string]Row{}
	for _, r := range rows {
		specs = append(specs, r.Spec)
		byID[r.Spec.ID] = r
	}
	withKube := func(n ClusterNode, match func(kubeapi.Node) bool) ClusterNode {
		if a, ok := recipes.NodeAddr(n.Row.Spec); ok {
			n.Addr = a.String()
		}
		for _, k := range kube {
			if match(k) {
				n.Registered, n.Ready, n.Version = true, k.Ready(), k.Status.NodeInfo.KubeletVersion
				break
			}
		}
		return n
	}
	var out []ClusterNode
	for _, r := range rows {
		if r.Spec.Recipe == recipes.Kubernetes {
			out = append(out, withKube(ClusterNode{Row: r, Server: true}, func(k kubeapi.Node) bool {
				_, ok := k.Metadata.Labels[controlPlaneLabel]
				return ok
			}))
			break
		}
	}
	for _, a := range recipes.Agents(specs) {
		name := a.Hostname
		if name == "" {
			name = a.Name
		}
		out = append(out, withKube(ClusterNode{Row: byID[a.ID]}, func(k kubeapi.Node) bool { return k.Name() == name }))
	}
	return out
}

// agentsOf are the IDs of the cluster's agents in specs.
func agentsOf(specs []sandbox.Spec) []string {
	var ids []string
	for _, a := range recipes.Agents(specs) {
		ids = append(ids, a.ID)
	}
	return ids
}

// forAgents calls fn with each of the cluster's agents and its state, as
// the cluster's server is paused, resumed, started or stopped.
func (m *Model) forAgents(fn func(id string, st sandbox.State)) {
	for _, a := range agentsOf(ggui.Peek(m.Specs)) {
		fn(a, m.sup.State(a))
	}
}

// AddNodeBlocked says why a node cannot be added now, or "" when one can:
// the cluster's server must run, with rungv's control, which reads its
// token.
func AddNodeBlocked(rows []Row) string {
	var specs []sandbox.Spec
	for _, r := range rows {
		specs = append(specs, r.Spec)
	}
	for _, r := range rows {
		if r.Spec.Recipe != recipes.Kubernetes {
			continue
		}
		switch {
		case r.State.Status != sandbox.Running || r.State.Paused:
			return tr("Start the cluster to add a node.")
		case !r.State.Control:
			return tr("Adding nodes needs rungvx 0.20 or later.")
		case recipes.NextAgent(specs) == 0:
			return tr("The cluster has as many nodes as its network holds.")
		}
		return ""
	}
	return tr("Set up Kubernetes to add a node.")
}

// AddNode adds an agent to the cluster and starts it. The first one moves
// the server onto the nodes' network, which restarts it: the user is asked
// first.
func (m *Model) AddNode() {
	if ggui.Peek(m.AddingNode) {
		return
	}
	rows := ggui.Peek(m.Rows)
	if why := AddNodeBlocked(rows); why != "" {
		m.toastError(tr("Could not add a node"), errors.New(why))
		return
	}
	server, _ := m.RecipeSandbox(rows, recipes.Kubernetes)
	if _, on := recipes.NodeAddr(server.Spec); on {
		m.addAgent(server.Spec.ID)
		return
	}
	m.Offer(tr("Add a node?"),
		tr("The cluster's server restarts to join the network its nodes share, and its pods restart with it. Then the new node joins."),
		tr("Restart and add"), func() {
			if ggui.Peek(m.AddingNode) {
				return
			}
			m.AddingNode.Set(true)
			moved := recipes.WithNodeNetwork(server.Spec)
			m.replaceSpec(moved)
			timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
			m.async(func(context.Context) error { return m.sup.Stop(moved.ID, timeout) }, func(error) {
				m.StartSandbox(moved.ID)
				m.addAgent(moved.ID)
			})
		})
}

// nodeTokenWait is how long adding a node waits for the server to be up
// and its token to be there.
const nodeTokenWait = 4 * time.Minute

// addAgent reads the join token from the running server serverID, as soon
// as it is up, then saves and starts a new agent.
func (m *Model) addAgent(serverID string) {
	m.AddingNode.Set(true)
	toast := m.Toasts.Push(ui.Toast(tr("Adding a node"), tr("Waiting for the cluster's server…")).Loading())
	var token string
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, nodeTokenWait)
		defer cancel()
		for {
			if ctl, ok := m.sandboxControl(serverID); ok && m.sup.State(serverID).Status == sandbox.Running {
				var err error
				if token, err = nodenet.ServerToken(ctx, ctl); err == nil {
					return nil
				}
			}
			select {
			case <-ctx.Done():
				return errors.New(tr("The cluster's server did not come up."))
			case <-time.After(2 * time.Second):
			}
		}
	}, func(err error) {
		m.AddingNode.Set(false)
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Could not add a node"), err.Error()).Error())
			return
		}
		specs := ggui.Peek(m.Specs)
		server, ok := m.spec(serverID)
		n := recipes.NextAgent(specs)
		if !ok || n == 0 {
			m.Toasts.Update(toast, ui.Toast(tr("Could not add a node"), AddNodeBlocked(ggui.Peek(m.Rows))).Error())
			return
		}
		agent := recipes.KubernetesAgentNode(server, n, token)
		agent.Name = uniqueName(specs, agent.Name)
		agent.Hostname = agent.Name
		m.saveSpecs(append(slices.Clone(specs), agent))
		m.StartSandbox(agent.ID)
		m.Toasts.Update(toast, ui.Toast(tr("Added %s", agent.Name), tr("It joins the cluster in a minute or so.")).Success())
	})
}

// replaceSpec saves spec in place of the spec with its ID.
func (m *Model) replaceSpec(spec sandbox.Spec) {
	specs := slices.Clone(ggui.Peek(m.Specs))
	for i, s := range specs {
		if s.ID == spec.ID {
			specs[i] = spec
		}
	}
	m.saveSpecs(specs)
}

// AskRemoveNode confirms and removes an agent: its sandbox stops, its node
// leaves the cluster, and its sandbox and volume are deleted.
func (m *Model) AskRemoveNode(id string) {
	spec, ok := m.spec(id)
	if !ok || spec.Recipe != recipes.KubernetesAgent {
		return
	}
	m.Confirm(tr("Remove %s?", spec.Name),
		tr("Its pods are made again on the other nodes, if a controller keeps them. Its sandbox and its volume are deleted."),
		tr("Remove node"), func() { m.removeAgents([]sandbox.Spec{spec}, true) })
}

// removeAgents stops the agents, deletes their nodes from the cluster when
// deleteNodes says to and its API server answers, and deletes their
// sandboxes and volumes.
func (m *Model) removeAgents(agents []sandbox.Spec, deleteNodes bool) {
	if len(agents) == 0 {
		return
	}
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	m.async(func(ctx context.Context) error {
		var errs []error
		for _, a := range agents {
			errs = append(errs, m.sup.Stop(a.ID, timeout))
		}
		if _, err := os.Stat(recipes.Kubeconfig()); deleteNodes && err == nil {
			if c, err := kubeapi.Open(recipes.Kubeconfig()); err == nil {
				for _, a := range agents {
					_ = c.DeleteNode(ctx, a.Hostname) // a cluster that is down keeps it NotReady
				}
				c.Close()
			}
		}
		for _, a := range agents {
			for _, v := range a.Volumes() {
				if c := m.Client(); c != nil {
					errs = append(errs, c.RemoveVolume(ctx, v))
				}
			}
		}
		return errors.Join(errs...)
	}, func(err error) {
		ids := map[string]bool{}
		for _, a := range agents {
			ids[a.ID] = true
			m.sup.Forget(a.ID)
			m.setResume(a.ID, false)
		}
		m.saveSpecs(slices.DeleteFunc(slices.Clone(ggui.Peek(m.Specs)), func(s sandbox.Spec) bool { return ids[s.ID] }))
		if err != nil {
			m.toastError(tr("Could not remove every part of the node"), err)
		}
		m.LoadVolumes()
		m.LoadPods()
	})
}
