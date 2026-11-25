package ui

import (
	"context"
	"errors"
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
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/term"
)

func sandboxTermTarget(id string) string   { return "sandbox:" + id }
func containerTermTarget(id string) string { return "container:" + id }

func isPausedSweepErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sandbox.ErrPaused) ||
		rungv.ControlCode(err) == rungv.CodeAlreadyPaused ||
		strings.Contains(strings.ToLower(err.Error()), "paused")
}

// The tabs of the sandboxes', containers' and pods' details that hold a terminal.
const (
	SandboxTerminalTab   = 4
	sandboxTerminalTab   = SandboxTerminalTab
	containerTerminalTab = 2
	podTerminalTab       = 2
)

// sandboxShellTarget decides what the Sandboxes page's Terminal tab runs or
// explains for row r.
func sandboxShellTarget(r Row, features rungv.Features, offline bool, start func(cols, rows uint16) (term.Program, error)) shellTarget {
	if !r.State.Status.Active() || r.State.Status == sandbox.Stopping {
		return shellTarget{
			Why:     tr("The sandbox is not running"),
			WhyText: tr("Start it to open a shell in it."),
		}
	}
	if r.State.Paused {
		return shellTarget{
			Why:     tr("The sandbox is paused"),
			WhyText: tr("Resume it to open a shell in it."),
		}
	}
	if !r.State.Control {
		if features.Control {
			return shellTarget{
				Why:     tr("Restart the sandbox to open a shell"),
				WhyText: tr("It started before rungvx was updated. Restart it to open a shell, pause it, or change its resources while it runs."),
			}
		}
		return shellTarget{
			Why:     tr("Needs rungvx v0.20 or later"),
			WhyText: tr("Update rungvx in Settings to open a shell in a running sandbox, pause it, and change its resources while it runs."),
		}
	}
	t := shellTarget{
		Key:      r.Spec.ID + "@" + r.State.StartedAt.UTC().Format(time.RFC3339Nano),
		TargetID: sandboxTermTarget(r.Spec.ID),
	}
	if offline {
		t.Start = cannedShell(containerTranscript(r.Spec.Name))
		return t
	}
	t.Start = start
	return t
}

// sandboxShell is the shell the Sandboxes page's terminal runs: in the chosen
// sandbox while its Terminal tab shows and the sandbox runs with a control socket.
func (m *Model) sandboxShell() ggui.Readable[shellTarget] {
	return ggui.Derived(func() shellTarget {
		if m.Page.Get() != PageSandboxes || m.Detail.Get() != sandboxTerminalTab {
			return shellTarget{}
		}
		id := m.Selected.Get()
		if id == "" {
			return shellTarget{}
		}
		var row Row
		for _, r := range m.Rows.Get() {
			if r.Spec.ID == id {
				row = r
				break
			}
		}
		if row.Spec.ID == "" {
			return shellTarget{}
		}
		features := m.RungvFeatures.Get()
		return sandboxShellTarget(row, features, m.Offline, func(cols, rows uint16) (term.Program, error) {
			return m.startSandboxShell(id, cols, rows)
		})
	}).WithEqual(shellTarget.Equal)
}

func (m *Model) startSandboxShell(id string, cols, rows uint16) (term.Program, error) {
	if m.sup != nil && m.sup.State(id).Paused {
		return nil, unavailable{tr("The sandbox is paused"), tr("Resume it to open a shell in it.")}
	}
	ctl, ok := m.sandboxControl(id)
	if !ok {
		return nil, sandbox.ErrNoControl
	}
	return rungvShell(ctl, cols, rows, func(mark string) {
		m.recordPendingSweep(sandboxTermTarget(id), mark)
	})
}

// rungvShell starts an interactive shell in a running sandbox via rungv exec -it
// over a PTY/ConPTY, and sweeps its marked guest processes when closed.
func rungvShell(ctl sandbox.Control, cols, rows uint16, onPausedSweep func(mark string)) (term.Program, error) {
	mark := term.NewMark()
	argv := ctl.ExecArgv(rungv.ExecOptions{
		Argv:        term.ShellCommand(mark),
		Interactive: true,
		TTY:         true,
	})
	if len(argv) == 0 {
		return nil, sandbox.ErrNoControl
	}
	env := term.Env(withoutOverrides(os.Environ()))
	prog, err := term.Start(argv, env, cols, rows)
	if err != nil {
		return nil, err
	}
	return term.Swept(prog, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sweepSandboxMark(ctx, ctl, mark); isPausedSweepErr(err) && onPausedSweep != nil {
			onPausedSweep(mark)
		}
	}), nil
}

func sweepSandboxMark(ctx context.Context, ctl sandbox.Control, mark string) error {
	proc, err := ctl.Exec(ctx, sandbox.ExecOptions{
		ExecOptions: rungv.ExecOptions{
			Argv:   term.SweepCommand(mark),
			Detach: true,
		},
	})
	if err != nil {
		return err
	}
	if proc != nil {
		_, waitErr := proc.Wait()
		_ = proc.Close()
		return waitErr
	}
	return nil
}

// drainSandboxSweeps runs any sweeps deferred while sandbox id was paused.
func (m *Model) drainSandboxSweeps(ctx context.Context, id string) {
	marks := m.takePendingSweeps(sandboxTermTarget(id))
	if len(marks) == 0 {
		return
	}
	ctl, ok := m.sandboxControl(id)
	if !ok {
		return
	}
	for _, mark := range marks {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = sweepSandboxMark(sctx, ctl, mark)
		cancel()
	}
}

// drainContainerSweeps runs any sweeps deferred while container id was paused.
func (m *Model) drainContainerSweeps(ctx context.Context, c *dockerapi.Client, id string) {
	marks := m.takePendingSweeps(containerTermTarget(id))
	for _, mark := range marks {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.ExecDetached(sctx, id, term.SweepCommand(mark))
		cancel()
	}
}

// containerShellTarget decides what the Containers page's Terminal tab runs or
// explains for container c.
func containerShellTarget(c dockerapi.Container, sandboxEngine bool, features rungv.Features, canControl, offline bool, start func(cols, rows uint16) (term.Program, error)) shellTarget {
	if sandboxEngine && !features.Control {
		return shellTarget{
			Why:     tr("Needs rungvx v0.20 or later"),
			WhyText: tr("Container exec in the sandbox engine needs rungvx v0.20 or later. Update rungvx in Settings, then restart the container."),
		}
	}
	if c.State == "paused" {
		return shellTarget{
			Why:     tr("The container is paused"),
			WhyText: tr("Resume it to open a shell in it."),
		}
	}
	if c.State != "running" {
		return shellTarget{
			Why:     tr("The container is not running"),
			WhyText: tr("Start it to open a shell in it."),
		}
	}
	if sandboxEngine && !canControl {
		return shellTarget{
			Why:     tr("Restart the container to open a shell"),
			WhyText: tr("It started before rungvx was updated to v0.20."),
		}
	}
	t := shellTarget{Key: c.ID, TargetID: containerTermTarget(c.ID)}
	if offline {
		t.Start = cannedShell(containerTranscript(c.Name()))
		return t
	}
	t.Start = start
	return t
}

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
		se := m.Settings.Get().SandboxEngine
		var features rungv.Features
		var canControl bool
		if se {
			features = m.engineFeatures()
			canControl = m.canControlContainer(c.ID)
		}
		id, host := c.ID, m.dockerBackendHost()
		return containerShellTarget(c, se, features, canControl, m.Offline, func(cols, rows uint16) (term.Program, error) {
			return dockerShell(host, id, cols, rows, func(mark string) {
				m.recordPendingSweep(containerTermTarget(id), mark)
			})
		})
	}).WithEqual(shellTarget.Equal)
}

// dockerShell starts a shell in the container id of the engine at host,
// marked so that closing it ends what it started there.
func dockerShell(host, id string, cols, rows uint16, onPausedSweep func(mark string)) (term.Program, error) {
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
		if strings.Contains(strings.ToLower(err.Error()), "is paused") {
			return nil, unavailable{tr("The container is paused"), tr("Resume it to open a shell in it.")}
		}
		return nil, err
	}
	return term.Swept(exec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.ExecDetached(ctx, id, term.SweepCommand(mark)); isPausedSweepErr(err) && onPausedSweep != nil {
			onPausedSweep(mark)
		}
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
