package ui

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// The kubectl context is set up, checked, made stale, updated and switched
// back with the kubectl on this machine, when there is one, in a home
// directory of the test's own: what the fake runner of the other tests
// stands in for. It never sees the user's kubeconfig.
func TestKubectlLinkWithTheRealKubectl(t *testing.T) {
	if testing.Short() {
		t.Skip("runs kubectl")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the home directory is not moved on Windows")
	}
	cli, err := rungv.FindTool("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "never-read")) // runCLI drops it
	ctx := context.Background()
	run := func(args ...string) ([]byte, error) { return runCLI(ctx, cli, args...) }
	must := func(args ...string) {
		t.Helper()
		if out, err := run(args...); err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
	}
	read := func() CLIContext {
		t.Helper()
		c := readKubeContext(ctx, cli, runCLI)
		if c.Err != "" {
			t.Fatalf("readKubeContext: %s", c.Err)
		}
		return c
	}

	// No kubeconfig of the cluster's yet.
	if c := readKubeContext(ctx, cli, runCLI); !c.Pending {
		t.Errorf("no cluster kubeconfig: %+v", c)
	}

	// The cluster's, as k3s writes it.
	writeCluster := func(cert string) {
		t.Helper()
		b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
		cfg := "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: " + b64("CA") +
			"\n    server: https://127.0.0.1:6443\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\n" +
			"current-context: default\nkind: Config\npreferences: {}\nusers:\n- name: default\n  user:\n    client-certificate-data: " + b64(cert) +
			"\n    client-key-data: " + b64("KEY") + "\n"
		if err := os.MkdirAll(recipes.KubeconfigDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recipes.Kubeconfig(), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCluster("CERT")

	// No kubeconfig of the user's either: kubectl prints an empty one.
	if c := read(); c.Exists || c.Current != "" || len(c.Contexts) != 0 {
		t.Errorf("no user kubeconfig: %+v", c)
	}

	// The user's, with a context of their own in use.
	must("config", "set-cluster", "kind-dev", "--server=https://127.0.0.1:50000")
	must("config", "set-context", "kind-dev", "--cluster=kind-dev")
	must("config", "use-context", "kind-dev")
	before, err := os.ReadFile(UserKubeconfig())
	if err != nil {
		t.Fatal(err)
	}
	if c := read(); c.Exists || c.Current != "kind-dev" || !slices.Equal(c.Contexts, []string{"kind-dev"}) {
		t.Errorf("before: %+v", c)
	}

	// Set up and use, as SetUpLink does.
	if err := kubeWrite(recipes.Kubeconfig())(ctx, run); err != nil {
		t.Fatal(err)
	}
	must("config", "use-context", KubeContextName)
	c := read()
	if !c.Exists || !c.InUse || c.Stale || c.Host != "https://127.0.0.1:6443" || c.Current != KubeContextName {
		t.Errorf("set up: %+v", c)
	}
	if kept, err := os.ReadFile(KubeconfigBackup()); err != nil || string(kept) != string(before) {
		t.Errorf("the backup: %v\n%s", err, kept)
	}
	mine, err := viewUserKubeconfig(run)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := mine.Entry(KubeContextName); !ok || e.Cert != base64.StdEncoding.EncodeToString([]byte("CERT")) || e.CA == "" || e.Key == "" {
		t.Errorf("the certificates were not embedded: %+v", e)
	}

	// The cluster made anew: the context is stale, and updating it mends it.
	writeCluster("NEW CERT")
	if c := read(); !c.Stale {
		t.Errorf("a new cluster: %+v", c)
	}
	if err := kubeWrite(recipes.Kubeconfig())(ctx, run); err != nil {
		t.Fatal(err)
	}
	if c := read(); c.Stale || !c.InUse {
		t.Errorf("updated: %+v", c)
	}
	if kept, _ := os.ReadFile(KubeconfigBackup()); string(kept) != string(before) {
		t.Error("the backup was written again")
	}

	// Back to the context before, and to none, as SwitchLinkBack does.
	must("config", "use-context", "kind-dev")
	if c := read(); c.InUse || c.Current != "kind-dev" || !c.Exists {
		t.Errorf("switched back: %+v", c)
	}
	must("config", "unset", "current-context")
	if c := read(); c.InUse || c.Current != "" {
		t.Errorf("no context: %+v", c)
	}
}

// viewUserKubeconfig reads the user's kubeconfig as kubectl prints it.
func viewUserKubeconfig(run func(...string) ([]byte, error)) (kubeconfig, error) {
	out, err := run("config", "view", "--raw", "-o", "json")
	if err != nil {
		return kubeconfig{}, err
	}
	return parseKubeconfig(out)
}
