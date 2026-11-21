package ui

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/term"
)

// The tabs of the containers' and pods' details that hold a terminal.
const (
	containerTerminalTab = 2
	podTerminalTab       = 2
)

// containerShell is the shell the Containers page's terminal runs: in the
// chosen container while its tab shows and the container runs.
//
// It runs through the engine's API, as docker exec -it does, so it needs
// no docker command and no context of docker's: only the engine.
func (m *Model) containerShell() ggui.Readable[shellTarget] {
	return ggui.Derived(func() shellTarget {
		c := m.Container.Get()
		if m.Page.Get() != PageContainers || m.ContainerDetail.Get() != containerTerminalTab || !m.ContainersOpen.Get() || c.ID == "" {
			return shellTarget{}
		}
		if m.Settings.Get().SandboxEngine {
			return shellTarget{
				Why:     tr("Not supported in the experimental sandbox engine"),
				WhyText: tr("Container exec is not supported in the experimental sandbox engine yet. Use docker run -it for an interactive shell."),
			}
		}
		if c.State != "running" {
			return shellTarget{Why: tr("The container is not running"), WhyText: tr("Start it to open a shell in it.")}
		}
		id, host := c.ID, m.dockerBackendHost()
		t := shellTarget{Key: id}
		if m.Offline {
			t.Start = cannedShell(containerTranscript(c.Name()))
			return t
		}
		t.Start = func(cols, rows uint16) (term.Program, error) { return dockerShell(host, id, cols, rows) }
		return t
	}).WithEqual(shellTarget.Equal)
}

// dockerShell starts a shell in the container id of the engine at host,
// marked so that closing it ends what it started there.
func dockerShell(host, id string, cols, rows uint16) (term.Program, error) {
	c, err := dockerapi.New(host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mark := term.NewMark()
	exec, err := c.Exec(ctx, id, term.ShellCommand(mark), nil, cols, rows)
	if err != nil {
		c.Close()
		return nil, err
	}
	return term.Swept(exec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.ExecDetached(ctx, id, term.SweepCommand(mark))
		c.Close()
	}), nil
}

// podShellContainer is the container of p a shell runs in: the one
// chosen when it runs, else the first that does, else the one chosen.
func podShellContainer(p kubeapi.Pod, chosen string) string {
	ctr := podLogContainer(p, chosen)
	if podContainerRunning(p, ctr) {
		return ctr
	}
	for _, c := range p.Spec.Containers {
		if podContainerRunning(p, c.Name) {
			return c.Name
		}
	}
	return ctr
}

func podContainerRunning(p kubeapi.Pod, name string) bool {
	st, ok := p.ContainerStatusOf(name)
	return ok && st.State.Running != nil
}

// podShell is the shell the Pods page's terminal runs: in the chosen
// container of the chosen pod while its tab shows and the container runs.
//
// It runs kubectl exec, given the cluster's own kubeconfig, so it reaches
// the cluster whatever context kubectl uses.
func (m *Model) podShell() ggui.Readable[shellTarget] {
	return ggui.Derived(func() shellTarget {
		p := m.Pod.Get()
		if m.Page.Get() != PagePods || m.PodDetail.Get() != podTerminalTab || !m.PodsOpen.Get() || p.Name() == "" {
			return shellTarget{}
		}
		ctr := podShellContainer(p, m.PodContainer.Get())
		if p.Metadata.Deleted != nil || !podContainerRunning(p, ctr) {
			return shellTarget{Why: tr("The container is not running"), WhyText: tr("A shell opens in a container while it runs.")}
		}
		ns, name := p.Namespace(), p.Name()
		t := shellTarget{Key: p.Key() + "/" + ctr + "@" + p.Metadata.UID}
		if m.Offline {
			t.Start = cannedShell(podTranscript(name))
			return t
		}
		file := recipes.Kubeconfig()
		t.Start = func(cols, rows uint16) (term.Program, error) { return kubectlShell(file, ns, name, ctr, cols, rows) }
		return t
	}).WithEqual(shellTarget.Equal)
}

// kubectlShell starts a shell in a container of a pod with kubectl exec,
// reaching the cluster through its kubeconfig file, and marked so that
// closing it ends what it started there.
func kubectlShell(file, ns, pod, ctr string, cols, rows uint16) (term.Program, error) {
	cli, ok := findCLI("kubectl", "")
	if !ok {
		return nil, unavailable{tr("The kubectl command was not found."), tr("Install kubectl, then check again.")}
	}
	mark := term.NewMark()
	at := []string{"--kubeconfig", file, "exec", "-n", ns, pod, "-c", ctr}
	env := term.Env(withoutOverrides(os.Environ()))
	argv := append(append([]string{cli}, at...), "-it", "--")
	prog, err := term.Start(append(argv, term.ShellCommand(mark)...), env, cols, rows)
	if err != nil {
		return nil, err
	}
	return term.Swept(prog, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = runCLI(ctx, cli, append(append(slices.Clone(at), "--"), term.SweepCommand(mark)...)...)
	}), nil
}

// ---- Made-up shells -----------------------------------------------------

// cannedShell starts a program that writes transcript and then waits,
// typing nowhere, until it is closed: the shell of the screens made with
// made-up state, which must start no process.
func cannedShell(transcript string) func(cols, rows uint16) (term.Program, error) {
	return func(uint16, uint16) (term.Program, error) {
		return &canned{out: strings.NewReader(strings.ReplaceAll(transcript, "\n", "\r\n")), closed: make(chan struct{})}, nil
	}
}

type canned struct {
	out    io.Reader
	closed chan struct{}
	once   sync.Once
}

func (c *canned) Read(p []byte) (int, error) {
	if n, err := c.out.Read(p); err == nil {
		return n, nil
	}
	<-c.closed
	return 0, io.EOF
}

func (c *canned) Write(p []byte) (int, error)    { return len(p), nil }
func (c *canned) Resize(_, _, _, _ uint16) error { return nil }
func (c *canned) Wait() int                      { <-c.closed; return 0 }
func (c *canned) EndOutput(time.Duration)        {}
func (c *canned) Kill()                          { c.Close() }
func (c *canned) Close() error                   { c.once.Do(func() { close(c.closed) }); return nil }
func containerTranscript(host string) string     { return shellTranscript(host, "/ # ") }
func podTranscript(host string) string           { return shellTranscript(host, "/app $ ") }
func shellTranscript(host, prompt string) string {
	return prompt + "uname -a\n" +
		"Linux " + host + " 6.12.8-gvx #1 SMP PREEMPT aarch64 Linux\n" +
		prompt + "ls -p\n" +
		"\x1b[1;34mbin/\x1b[0m    \x1b[1;34metc/\x1b[0m    \x1b[1;34mhome/\x1b[0m   \x1b[1;34mlib/\x1b[0m    \x1b[1;34musr/\x1b[0m    \x1b[1;34mvar/\x1b[0m    \x1b[1;32mentrypoint.sh\x1b[0m\n" +
		prompt + "ps\n" +
		"PID   USER     TIME  COMMAND\n" +
		"    1 root      0:00 nginx: master process nginx -g daemon off;\n" +
		"   30 nginx     0:00 nginx: worker process\n" +
		"   41 root      0:00 sh\n" +
		"   48 root      0:00 ps\n" +
		prompt
}
