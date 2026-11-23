//go:build !windows

package nodenet

// A real cluster of two nodes, the recipe's server and an agent, each in
// its own sandbox under the supervisor, on Network. The server first runs
// alone, off the network, as a recipe cluster made before nodes did, then
// moves onto it. Skipped unless GVX_NODENET_HARNESS=1; it uses a cache,
// volumes and directories of its own under /tmp/gvxd-nodenet, and the
// host port 18090.

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const harnessRoot = "/tmp/gvxd-nodenet"

func TestHarnessTwoNodes(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_NODENET_HARNESS") != "1" {
		t.Skip("set GVX_NODENET_HARNESS=1 to run a real two-node cluster")
	}
	bin, err := rungv.Locate(os.Getenv("GVX_RUNGV"))
	if err != nil {
		t.Skip("rungv not found")
	}
	cache := filepath.Join(harnessRoot, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	run, err := os.MkdirTemp(harnessRoot, "r")
	if err != nil {
		t.Fatal(err)
	}
	client := &rungv.Client{Path: bin, CacheDir: cache}
	if !client.Features(context.Background()).Control {
		t.Skip("adding nodes needs rungv's control (v0.20)")
	}
	output := filepath.Join(run, "output")
	network := &Network{Dir: filepath.Join(run, "net"), Client: func() *rungv.Client { return client }, Logf: t.Logf}
	sup := &sandbox.Supervisor{
		Client:    func() *rungv.Client { return client },
		DataDir:   filepath.Join(run, "d"),
		Prepare:   recipes.Prepare,
		StopGrace: recipes.StopGrace,
		Launch:    network.Launch,
	}
	var started []string
	t.Cleanup(func() {
		for _, id := range started {
			_ = sup.Stop(id, 30*time.Second)
		}
		waitFor(t, time.Minute, "the nodes to stop", func() bool {
			for _, id := range started {
				if sup.State(id).Status.Active() {
					return false
				}
			}
			return true
		})
		network.Close()
		_ = os.RemoveAll(run)
		_ = exec.Command(bin, "volume", "rm", "-cache-dir", cache, "gvxd-nodenet-k3s", "gvxd-nodenet-agent").Run()
	})

	server := recipes.KubernetesNode()
	server.Name = "gvxd-nodenet-k3s"
	server.Ports = []string{"127.0.0.1:18090:6443"}
	for i, m := range server.Mounts {
		switch m.Target {
		case "/var/lib/rancher":
			server.Mounts[i].Source = "gvxd-nodenet-k3s"
		case "/output":
			server.Mounts[i].Source = output
		}
	}
	start := func(spec sandbox.Spec) {
		t.Helper()
		if err := sup.Start(spec); err != nil {
			t.Fatalf("start %s: %v", spec.Name, err)
		}
		started = append(started, spec.ID)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("%s's last lines:\n%s", spec.Name, strings.Join(sup.Logs(spec.ID).Tail(40, sandbox.Stderr), "\n"))
			}
		})
		waitFor(t, 4*time.Minute, spec.Name+" to be ready", func() bool {
			st := sup.State(spec.ID)
			if !st.Status.Active() {
				t.Fatalf("%s ended: %+v\n%s", spec.Name, st, strings.Join(sup.Logs(spec.ID).Tail(30, sandbox.Stderr), "\n"))
			}
			return st.Status == sandbox.Running
		})
	}

	// A cluster of one, as the recipe made it before.
	start(server)
	if err := sup.Stop(server.ID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Minute, "the server to stop", func() bool { return !sup.State(server.ID).Status.Active() })

	// The server moves onto the network.
	server = recipes.WithNodeNetwork(server)
	start(server)
	ctl, ok := sup.Control(server.ID)
	if !ok {
		t.Fatal("no control of the server")
	}
	kubectl := func(args ...string) string {
		t.Helper()
		return execOut(t, ctl, append([]string{"kubectl"}, args...)...)
	}
	waitFor(t, 2*time.Minute, "the server's node to take its address", func() bool {
		return strings.Contains(kubectl("get", "nodes", "-o", "wide"), recipes.NodeServerAddr.String())
	})

	var token string
	waitFor(t, time.Minute, "the node token", func() bool {
		token, err = ServerToken(context.Background(), ctl)
		return err == nil
	})
	if !strings.HasPrefix(token, "K10") {
		t.Fatalf("token %q", token)
	}

	agent := recipes.KubernetesAgentNode(server, 1, token)
	agent.Name, agent.Hostname = "gvxd-nodenet-agent", "gvxd-nodenet-agent"
	agent.Mounts[0].Source = "gvxd-nodenet-agent"
	start(agent)
	waitFor(t, 3*time.Minute, "two Ready nodes", func() bool {
		return strings.Count(kubectl("get", "nodes"), " Ready ") == 2
	})
	t.Log(kubectl("get", "nodes", "-o", "wide"))

	// A pod on each node; the server's reaches the agent's, and the API
	// server reaches the agent's kubelet for its logs.
	for _, n := range []string{"gvxd-nodenet-agent", serverNode(t, kubectl)} {
		kubectl("run", "p-"+n, "--image=busybox:1.36", `--overrides={"spec":{"nodeName":"`+n+`"}}`, "--",
			"sh", "-c", `echo started; while true; do printf "HTTP/1.0 200 OK\r\n\r\nfrom `+n+`\n" | nc -l -p 8080; done`)
	}
	waitFor(t, 3*time.Minute, "the pods to run", func() bool {
		return strings.Count(kubectl("get", "pods"), "Running") == 2
	})
	if logs := kubectl("logs", "p-gvxd-nodenet-agent"); !strings.Contains(logs, "started") {
		t.Errorf("logs of the agent's pod: %q", logs)
	}
	ip := strings.TrimSpace(kubectl("get", "pod", "p-gvxd-nodenet-agent", "-o", "jsonpath={.status.podIP}"))
	var got string
	waitFor(t, 30*time.Second, "the agent's pod from the server's", func() bool {
		got = kubectl("exec", "p-"+serverNode(t, kubectl), "--", "wget", "-qO-", "-T", "3", "http://"+ip+":8080/")
		return strings.Contains(got, "from gvxd-nodenet-agent")
	})
}

// serverNode is the name of the server's node.
func serverNode(t *testing.T, kubectl func(...string) string) string {
	t.Helper()
	return strings.TrimSpace(kubectl("get", "nodes", "-l", "node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[0].metadata.name}"))
}

// execOut runs argv in the guest and returns what it printed, both
// streams.
func execOut(t *testing.T, ctl sandbox.Control, argv ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := ctl.Exec(ctx, sandbox.ExecOptions{ExecOptions: rungv.ExecOptions{Argv: argv}})
	if err != nil {
		t.Logf("exec %q: %v", argv, err)
		return ""
	}
	defer p.Close()
	var out bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range []io.Reader{p.Stdout(), p.Stderr()} {
		if r == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := io.ReadAll(r)
			mu.Lock()
			out.Write(b)
			mu.Unlock()
		}()
	}
	wg.Wait()
	_, _ = p.Wait()
	return out.String()
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Second)
	}
}
