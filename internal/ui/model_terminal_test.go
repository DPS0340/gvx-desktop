package ui

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/term"
)

func TestPodShellContainer(t *testing.T) {
	running := kubeapi.ContainerStatus{Name: "web", State: kubeapi.ContainerState{Running: &kubeapi.Running{}}}
	waiting := kubeapi.ContainerStatus{Name: "side"}
	p := kubeapi.Pod{Spec: kubeapi.PodSpec{
		InitContainers: []kubeapi.Container{{Name: "init"}},
		Containers:     []kubeapi.Container{{Name: "side"}, {Name: "web"}},
	}}
	p.Status.Containers = []kubeapi.ContainerStatus{waiting, running}
	for chosen, want := range map[string]string{"": "web", "web": "web", "side": "web", "init": "web", "gone": "web"} {
		if got := podShellContainer(p, chosen); got != want {
			t.Errorf("chosen %q: %q, want %q", chosen, got, want)
		}
	}
	// None runs: the one chosen, which the tab says is not running.
	p.Status.Containers = []kubeapi.ContainerStatus{waiting}
	if got := podShellContainer(p, "side"); got != "side" {
		t.Errorf("none running: %q", got)
	}
}

// The commands a container's shell is started and swept with.
func TestShellCommands(t *testing.T) {
	mark := term.NewMark()
	if len(mark) != 24 || mark == term.NewMark() {
		t.Fatalf("mark %q", mark)
	}
	start := strings.Join(term.ShellCommand(mark), " ")
	if !strings.Contains(start, term.MarkVar+"="+mark) || !strings.Contains(start, "exec bash") || !strings.Contains(start, "exec sh") {
		t.Errorf("starts with %q", start)
	}
	if sweep := strings.Join(term.SweepCommand(mark), " "); !strings.Contains(sweep, term.MarkVar+"="+mark) {
		t.Errorf("sweeps with %q", sweep)
	}
}

// TestAKubectlShell runs a shell in a pod's container as the Pods page's
// terminal does, through kubectl. It needs a cluster and a pod in it:
// GVX_TEST_KUBECONFIG names the cluster's kubeconfig, GVX_TEST_POD the
// pod as namespace/name/container.
func TestAKubectlShell(t *testing.T) {
	file, at := os.Getenv("GVX_TEST_KUBECONFIG"), strings.Split(os.Getenv("GVX_TEST_POD"), "/")
	if file == "" || len(at) != 3 || runtime.GOOS == "windows" {
		t.Skip("GVX_TEST_KUBECONFIG and GVX_TEST_POD name no pod to run a shell in")
	}
	ns, pod, ctr := at[0], at[1], at[2]
	start := func() *term.Session {
		prog, err := kubectlShell(file, ns, pod, ctr, 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		s, err := term.New(prog, term.Options{Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitText := func(s *term.Session, want string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(s.Text(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %q, the screen shows:\n%s", want, s.Text())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	s := start()
	defer s.Close()
	s.Write([]byte("echo $((6*7)); stty size; echo $TERM\r"))
	waitText(s, "\n42")
	waitText(s, "\n24 80")
	waitText(s, "\nxterm-256color")
	time.Sleep(300 * time.Millisecond)
	s.Resize(100, 30, 8, 16)
	time.Sleep(300 * time.Millisecond)
	s.Write([]byte("stty size\r"))
	waitText(s, "\n30 100")
	s.Write([]byte("exit 7\r"))
	select {
	case <-s.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("the shell did not exit")
	}
	if _, code := s.Exited(); code != 7 {
		t.Fatalf("exited with %d, want 7", code)
	}

	// Closed while it runs a command, the shell leaves nothing running in
	// the container, and no kubectl here.
	s2 := start()
	s2.Write([]byte("sleep 4242 & sleep 4243\r"))
	time.Sleep(time.Second)
	s2.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cli, _ := findCLI("kubectl", "")
		out, err := runCLI(ctx, cli, "--kubeconfig", file, "exec", "-n", ns, pod, "-c", ctr, "--", "ps", "-o", "pid,args")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "sleep 424") && !strings.Contains(string(out), " sh\n") && !strings.Contains(string(out), "bash") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell outlived its terminal:\n%s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// The Containers page's terminal runs a shell while its tab shows and its
// container runs, and ends it as either stops being so; one that ends on
// its own says so, and starts again on Restart.
func TestAContainerTerminalLivesWithItsTab(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	docker := recipes.DockerEngine()
	if err := st.SaveSandboxes([]sandbox.Spec{docker}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 900))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true // the shell is a made-up one
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()
	m.States.Set(map[string]sandbox.State{docker.ID: {Status: sandbox.Running}})
	web := dockerapi.Container{ID: "3f2a9c1b7d4e5f60", Names: []string{"/web"}, Image: "nginx:alpine", State: "running"}
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{web}})
	m.Go(PageContainers)
	p.Frame()

	live := func() []*term.Session {
		m.terms.mu.Lock()
		defer m.terms.mu.Unlock()
		var out []*term.Session
		for s := range m.terms.set {
			out = append(out, s)
		}
		return out
	}
	settle := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %s:\n%s", what, p.Semantics())
			}
			p.Frame()
			time.Sleep(5 * time.Millisecond)
		}
	}
	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, text) {
				return true
			}
		}
		return false
	}

	if n := len(live()); n != 0 {
		t.Fatalf("%d shells before the tab opened", n)
	}
	m.ContainerDetail.Set(containerTerminalTab)
	settle("a shell", func() bool { return len(live()) == 1 })
	s := live()[0]
	settle("its screen", func() bool { return strings.Contains(s.Text(), "uname -a") })

	// Another tab ends it.
	m.ContainerDetail.Set(0)
	settle("the shell to end", func() bool { return len(live()) == 0 })
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the shell runs on")
	}

	// Back on the tab, a new one; the container stopping ends it, and the
	// tab says why there is none.
	m.ContainerDetail.Set(containerTerminalTab)
	settle("a second shell", func() bool { return len(live()) == 1 && live()[0] != s })
	web.State = "exited"
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{web}})
	settle("the shell to end with its container", func() bool { return len(live()) == 0 })
	if !shows("The container is not running") {
		t.Errorf("the tab does not say the container stopped:\n%s", p.Semantics())
	}

	// Running again, a shell; one that ends on its own says so.
	web.State = "running"
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{web}})
	settle("a third shell", func() bool { return len(live()) == 1 })
	third := live()[0]
	third.Close() // as an exit would end it
	settle("the end to show", func() bool { return shows("The shell ended") })
	p.Tap("Restart the shell")
	settle("a shell after Restart", func() bool {
		l := live()
		return len(l) == 1 && l[0] != third
	})

	// Leaving the page ends it too, and quitting ends what is left.
	m.Go(PageDashboard)
	settle("the shell to end with the page", func() bool { return len(live()) == 0 })
	m.Go(PageContainers)
	settle("a shell on coming back", func() bool { return len(live()) == 1 })
	last := live()[0]
	m.CloseTerminals()
	select {
	case <-last.Done():
	default:
		t.Fatal("CloseTerminals left a shell running")
	}
}
