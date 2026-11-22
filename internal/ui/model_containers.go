package ui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// ContainersAccess is whether the Containers page can be used, and if not,
// why: its containers are the Docker Engine's, which answers only while its
// sandbox is running, or always when the experimental sandbox engine is on.
type ContainersAccess int

const (
	ContainersOpen     ContainersAccess = iota // the engine is running
	ContainersNoEngine                         // the Docker Engine recipe is not set up
	ContainersStarting                         // the engine's sandbox runs and dockerd is not ready yet
	ContainersStopped                          // the engine is stopped, stopping, exited or failed
	ContainersPaused                           // the engine's sandbox is paused
)

// containersAccess reads it from the sandboxes and settings.
func containersAccess(rows []Row, set store.Settings) ContainersAccess {
	if set.SandboxEngine {
		return ContainersOpen
	}
	return ContainersAccess(engineStage(rows, recipes.Docker))
}

// Reason says why the page cannot be used, or nothing when it can.
func (a ContainersAccess) Reason() string {
	switch a {
	case ContainersNoEngine:
		return tr("Set up the Docker Engine to manage its containers")
	case ContainersStarting:
		return tr("The Docker Engine is starting")
	case ContainersStopped:
		return tr("Start the Docker Engine to manage its containers")
	case ContainersPaused:
		return tr("Resume the Docker Engine to manage its containers")
	}
	return ""
}

// chosenContainer is the container with id, or the first when none has it.
func chosenContainer(list []dockerapi.Container, id string) dockerapi.Container {
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	if len(list) > 0 {
		return list[0]
	}
	return dockerapi.Container{}
}

// filterContainers keeps the containers whose name, image, ID or Compose
// project has q in it, in either case.
func filterContainers(list []dockerapi.Container, q string) []dockerapi.Container {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return list
	}
	var out []dockerapi.Container
	for _, c := range list {
		if strings.Contains(strings.ToLower(c.Name()+" "+c.Image+" "+c.ShortID()+" "+c.Project()), q) {
			out = append(out, c)
		}
	}
	return out
}

// containerRow is a row of the containers' table: a container, or the
// head of a Compose project, which its containers follow unless it is
// folded.
type containerRow struct {
	Project string // the head's project
	Head    bool
	C       dockerapi.Container   // the row's container, for one that is not a head
	Members []dockerapi.Container // the head's: every container of its project
	Folded  bool                  // the head's: its containers are hidden
}

// composeKey is the key of a project's head: no container's ID has a colon.
func composeKey(project string) string { return "compose:" + project }

// key is what the table keys the row by: its container's ID, or its
// project's composeKey.
func (r containerRow) key() string {
	if r.Head {
		return composeKey(r.Project)
	}
	return r.C.ID
}

// name is the row as a screen reader names it.
func (r containerRow) name() string {
	if r.Head {
		return tr("Compose project %s", r.Project)
	}
	return r.C.Name()
}

// containerRows lays the containers q leaves out as the table shows them:
// those of no Compose project first, as the engine lists them, then each
// project in the order of its name, its head followed by those of its
// containers q leaves unless it is folded. A project shows while any of
// its containers is left; its head counts all of them.
func containerRows(list []dockerapi.Container, q string, folded map[string]bool) []containerRow {
	var out, grouped []containerRow
	members := map[string][]dockerapi.Container{}
	for _, c := range list {
		if p := c.Project(); p != "" {
			members[p] = append(members[p], c)
		}
	}
	shown := map[string][]dockerapi.Container{}
	for _, c := range filterContainers(list, q) {
		p := c.Project()
		if p == "" {
			out = append(out, containerRow{C: c})
			continue
		}
		shown[p] = append(shown[p], c)
	}
	for _, p := range slices.Sorted(maps.Keys(shown)) {
		grouped = append(grouped, containerRow{Project: p, Head: true, Members: members[p], Folded: folded[p]})
		if folded[p] {
			continue
		}
		for _, c := range shown[p] {
			grouped = append(grouped, containerRow{Project: p, C: c})
		}
	}
	return append(out, grouped...)
}

// projectCounts counts a project's running containers and all of them.
func projectCounts(members []dockerapi.Container) (running, total int) {
	for _, c := range members {
		if c.State == "running" {
			running++
		}
	}
	return running, len(members)
}

// ToggleProject folds a Compose project's containers under its head, or
// unfolds them.
func (m *Model) ToggleProject(project string) {
	m.ComposeFolded.Update(func(f map[string]bool) map[string]bool {
		f = maps.Clone(f)
		if f == nil {
			f = map[string]bool{}
		}
		if f[project] {
			delete(f, project)
		} else {
			f[project] = true
		}
		return f
	})
}

// projectTargets are the containers of a project an action on it acts on:
// start those that are not running (including paused, which are unpaused),
// stop those that are running or paused, and restart or remove all of them,
// as docker compose does.
func projectTargets(action string, members []dockerapi.Container) []dockerapi.Container {
	var out []dockerapi.Container
	for _, c := range members {
		running := c.State == "running" || c.State == "restarting"
		switch {
		case action == "start" && !running && c.State != "removing",
			action == "stop" && (running || c.State == "paused"),
			action == "restart", action == "remove":
			out = append(out, c)
		}
	}
	return out
}

// ProjectAction runs start, stop, restart or remove on the containers of
// a Compose project, all at once, and says which failed.
func (m *Model) ProjectAction(action, project string, members []dockerapi.Container) {
	targets := projectTargets(action, members)
	if len(targets) == 0 || m.Offline {
		return
	}
	host := m.dockerBackendHost()
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		var mu sync.Mutex
		var wg sync.WaitGroup
		var errs []error
		for _, t := range targets {
			wg.Go(func() {
				act := action
				if act == "start" && t.State == "paused" {
					act = "unpause"
				}
				if err := dockerDo(ctx, c, act, t.ID); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
					mu.Unlock()
				} else if act == "unpause" {
					m.drainContainerSweeps(ctx, c, t.ID)
				}
			})
		}
		wg.Wait()
		return errors.Join(errs...)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not %s project %s", action, project), err)
		}
		m.LoadDocker()
		if ggui.Peek(m.Page) == PageContainers && ggui.Peek(m.Container).Project() == project {
			m.LoadContainerLogs()
		}
	})
}

// AskRemoveProject confirms and removes the containers of a Compose
// project.
func (m *Model) AskRemoveProject(project string, members []dockerapi.Container) {
	m.Confirm(tr("Remove project %s?", project),
		trn(len(members), "Its %d container is stopped and removed from the engine. Its volumes, networks and images stay.",
			"Its %d containers are stopped and removed from the engine. Its volumes, networks and images stay."),
		tr("Remove containers"), func() { m.ProjectAction("remove", project, members) })
}

// SelectContainer shows a container in the details under the table.
func (m *Model) SelectContainer(id string) {
	if ggui.Peek(m.SelectedContainer) == id {
		return
	}
	m.SelectedContainer.Set(id)
}

// ShowContainerLogs opens the Containers page on a container's logs.
func (m *Model) ShowContainerLogs(id string) {
	m.SelectContainer(id)
	m.ContainerDetail.Set(0)
	m.Go(PageContainers)
}

// ContainerLogs are the last lines a container wrote.
type ContainerLogs struct {
	ID     string // the container they are of
	Lines  []sandbox.LogLine
	Err    string
	Loaded bool
}

// Equal makes ContainerLogs comparable for signals: lines are numbered by
// their time, so the same ends hold the same lines.
func (l ContainerLogs) Equal(o ContainerLogs) bool {
	if l.ID != o.ID || l.Err != o.Err || l.Loaded != o.Loaded || len(l.Lines) != len(o.Lines) {
		return false
	}
	n := len(l.Lines)
	return n == 0 || l.Lines[0].Seq == o.Lines[0].Seq && l.Lines[n-1].Seq == o.Lines[n-1].Seq
}

// containerLogTail is how many of its last lines a container's logs show.
const containerLogTail = 500

// containerLogLines makes the engine's lines into the log viewer's.
func containerLogLines(lines []dockerapi.LogLine) []sandbox.LogLine {
	return timedLines(len(lines), func(i int) (time.Time, string, bool) { return lines[i].Time, lines[i].Text, lines[i].Stderr })
}

// timedLines makes n lines an engine read, each its time, text and
// whether it came from stderr, into the log viewer's. Each is numbered by
// its time, in nanoseconds, and one past the line before it when that is
// no later: a line keeps its number as the lines before it fall out of
// the tail.
func timedLines(n int, line func(i int) (time.Time, string, bool)) []sandbox.LogLine {
	out := make([]sandbox.LogLine, n)
	var last int64
	for i := range n {
		at, text, stderr := line(i)
		seq := at.UnixNano()
		if at.IsZero() || seq <= last {
			seq = last + 1
		}
		last = seq
		stream := sandbox.Stdout
		if stderr {
			stream = sandbox.Stderr
		}
		out[i] = sandbox.LogLine{Seq: seq, Time: at.Local(), Stream: stream, Text: text}
	}
	return out
}

// LoadContainerLogs reads the last lines of the chosen container. Asked for
// while a reading runs, it reads them again after it.
func (m *Model) LoadContainerLogs() {
	if m.Offline {
		return // the lines shown are made up
	}
	ctr := ggui.Peek(m.Container)
	if ctr.ID == "" || ggui.Peek(m.ContainersAccess) != ContainersOpen {
		m.ContainerLogs.Set(ContainerLogs{})
		return
	}
	if m.logsLoading {
		m.logsAgain = true
		return
	}
	m.logsLoading = true
	if ggui.Peek(m.ContainerLogs).ID != ctr.ID {
		m.ContainerLogs.Set(ContainerLogs{ID: ctr.ID})
	}
	host := m.dockerBackendHost()
	var lines []dockerapi.LogLine
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		lines, err = c.Logs(ctx, ctr.ID, containerLogTail)
		return err
	}, func(err error) {
		m.logsLoading = false
		if m.logsAgain {
			m.logsAgain = false
			m.LoadContainerLogs()
			return
		}
		if ggui.Peek(m.Container).ID != ctr.ID {
			return
		}
		l := ContainerLogs{ID: ctr.ID, Lines: containerLogLines(lines), Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.ContainerLogs.Set(l)
	})
}

// followsContainerLogs reports whether the chosen container's logs are on
// screen, and are to be read as often as the containers are.
func (m *Model) followsContainerLogs() bool {
	return m.Page.Get() == PageContainers && m.ContainerDetail.Get() == 0 && m.Container.Get().ID != ""
}
