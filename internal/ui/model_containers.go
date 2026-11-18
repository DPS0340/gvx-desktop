package ui

import (
	"context"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// ContainersAccess is whether the Containers page can be used, and if not,
// why: its containers are the Docker Engine's, which answers only while its
// sandbox is running.
type ContainersAccess int

const (
	ContainersOpen     ContainersAccess = iota // the engine is running
	ContainersNoEngine                         // the Docker Engine recipe is not set up
	ContainersStarting                         // the engine's sandbox runs and dockerd is not ready yet
	ContainersStopped                          // the engine is stopped, stopping, exited or failed
)

// containersAccess reads it from the sandboxes.
func containersAccess(rows []Row) ContainersAccess {
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

// filterContainers keeps the containers whose name, image or ID has q in
// it, in either case.
func filterContainers(list []dockerapi.Container, q string) []dockerapi.Container {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return list
	}
	var out []dockerapi.Container
	for _, c := range list {
		if strings.Contains(strings.ToLower(c.Name()+" "+c.Image+" "+c.ShortID()), q) {
			out = append(out, c)
		}
	}
	return out
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
	host := m.dockerHost()
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
