//go:build !windows

package ui

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// fakeNodeRungv writes a rungv v0.20 that runs no sandbox: a run says
// k3s's ready lines, a server's and an agent's, notes its arguments in
// args, and waits for its interrupt.
func fakeNodeRungv(t *testing.T) (path, args string) {
	t.Helper()
	dir := t.TempDir()
	path, args = filepath.Join(dir, "rungv"), filepath.Join(dir, "args")
	t.Cleanup(func() { rungv.ForgetFeatures(path) })
	script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -control-socket string"
  echo "  -control-token-file string"
  exit 2
fi
[ "$1" = "run" ] || exit 0
printf '%s\n' "$*" >> "` + args + `"
echo 'k3s is up and running' >&2
echo 'Annotations and labels have been set successfully on node: n' >&2
trap 'exit 0' INT TERM
while true; do sleep 0.05; done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, args
}

// tokenControl is a server's control that gives its node token.
type tokenControl struct {
	sandbox.Control
	token string
}

func (c tokenControl) CopyFrom(context.Context, string, rungv.CopyOptions) (io.ReadCloser, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "node-token", Mode: 0o600, Size: int64(len(c.token) + 1)})
	_, _ = tw.Write([]byte(c.token + "\n"))
	_ = tw.Close()
	return io.NopCloser(&b), nil
}

// Adding a node moves the server onto the nodes' network, restarting it,
// and starts an agent with the server's token, on the network too; the
// cluster then starts and stops whole, and a node is removed alone.
func TestAddNode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rungvPath, argsFile := fakeNodeRungv(t)
	st := store.Store{Dir: t.TempDir()}
	m, p := launch(t, st, rungvPath)
	m.sup.StopGrace = nil // the stand-in ends at its interrupt
	m.controlOf = func(string) (sandbox.Control, bool) { return tokenControl{token: "K10cafe::server:secret"}, true }

	m.CreateFromRecipe(recipes.Kubernetes)
	server, _ := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Kubernetes)
	id := server.Spec.ID
	until(t, p, "the server to be up", func() bool { st := m.sup.State(id); return st.Status == sandbox.Running && st.Control })
	firstPID := m.sup.State(id).PID

	m.AddNode()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("the first node did not ask before restarting the server")
	}
	m.RunConfirmed()
	var agent sandbox.Spec
	until(t, p, "the agent to be up", func() bool {
		agents := recipes.Agents(ggui.Peek(m.Specs))
		if len(agents) != 1 {
			return false
		}
		agent = agents[0]
		return m.sup.State(agent.ID).Status == sandbox.Running
	})
	if s, _ := m.spec(id); !strings.Contains(s.Command, "--node-ip=10.88.0.2") {
		t.Errorf("the server's command %q", s.Command)
	}
	if st := m.sup.State(id); st.Status != sandbox.Running || st.PID == firstPID {
		t.Errorf("the server did not restart: %+v", st)
	}
	if agent.Name != "k3s-agent-1" || !slices.Contains(agent.Env, "K3S_TOKEN=K10cafe::server:secret") {
		t.Errorf("agent %+v", agent)
	}
	runs, _ := os.ReadFile(argsFile)
	lines := strings.Split(strings.TrimSpace(string(runs)), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d runs, want the server twice and the agent:\n%s", len(lines), runs)
	}
	if strings.Contains(lines[0], guestcfg.Path) {
		t.Errorf("the single-node server ran netinit: %s", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, guestcfg.Path) || !strings.Contains(l, "GVX_NET=") || !strings.Contains(l, "-host-dns") {
			t.Errorf("a node ran off the network: %s", l)
		}
	}

	// A second node needs no restart.
	m.ConfirmOpen.Set(false) // as the dialog closes
	m.AddNode()
	if ggui.Peek(m.ConfirmOpen) {
		t.Fatal("the second node asked again")
	}
	until(t, p, "the second agent", func() bool {
		agents := recipes.Agents(ggui.Peek(m.Specs))
		return len(agents) == 2 && m.sup.State(agents[1].ID).Status == sandbox.Running
	})

	// The cluster stops and starts whole.
	m.StopSandbox(id)
	until(t, p, "the cluster to stop", func() bool {
		for _, s := range ggui.Peek(m.Specs) {
			if recipes.IsKubernetesNode(s) && m.sup.State(s.ID).Status.Active() {
				return false
			}
		}
		return true
	})
	m.StartSandbox(id)
	until(t, p, "the cluster to start", func() bool {
		for _, s := range ggui.Peek(m.Specs) {
			if recipes.IsKubernetesNode(s) && m.sup.State(s.ID).Status != sandbox.Running {
				return false
			}
		}
		return true
	})

	// A node goes alone.
	m.AskRemoveNode(agent.ID)
	m.RunConfirmed()
	until(t, p, "the node to go", func() bool { return len(recipes.Agents(ggui.Peek(m.Specs))) == 1 })
	if m.sup.State(agent.ID).Status.Active() {
		t.Error("the removed node still runs")
	}
	if !m.sup.State(id).Status.Active() {
		t.Error("the server stopped with the node")
	}

	// Deleting the server deletes the cluster.
	m.AskDelete(id)
	m.RunConfirmed()
	until(t, p, "the cluster to go", func() bool {
		return !slices.ContainsFunc(ggui.Peek(m.Specs), recipes.IsKubernetesNode)
	})
}
