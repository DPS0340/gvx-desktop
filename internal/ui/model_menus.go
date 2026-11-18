package ui

import (
	"slices"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// maxMenuContainers is how many containers the Docker menu lists; the
// Containers page lists them all.
const maxMenuContainers = 10

// EngineState is a recipe's engine as the menus tell of it.
type EngineState struct {
	ID       string // empty when the recipe is not set up
	Status   sandbox.Status
	ExitCode int
}

// MenuState is what the Docker and Kubernetes menus tell. Equal states make
// the same menus, so the menus are built again only when it changes: not
// for a line of a log, nor for a container's uptime.
type MenuState struct {
	Docker, Kube EngineState
	// Containers are the ones the Docker menu lists, running first, and
	// More the number left out.
	Containers []dockerapi.Container
	More       int
	Listed     bool // the running engine answered with its containers
	ListErr    bool // the running engine did not answer
	DockerLink LinkView
	KubeLink   LinkView
}

// Equal makes MenuState comparable for signals. Containers are equal when
// the menu tells the same of them.
func (s MenuState) Equal(o MenuState) bool {
	return s.Docker == o.Docker && s.Kube == o.Kube && s.More == o.More && s.Listed == o.Listed && s.ListErr == o.ListErr &&
		s.DockerLink.Equal(o.DockerLink) && s.KubeLink.Equal(o.KubeLink) &&
		slices.EqualFunc(s.Containers, o.Containers, func(a, b dockerapi.Container) bool {
			return a.ID == b.ID && a.Name() == b.Name() && a.State == b.State
		})
}

// menuState sums up for the menus the sandboxes, the Docker Engine's
// containers and the contexts.
func menuState(rows []Row, containers Listing[dockerapi.Container], docker, kube LinkView) MenuState {
	engine := func(recipe string) EngineState {
		for _, r := range rows {
			if r.Spec.Recipe == recipe {
				return EngineState{ID: r.Spec.ID, Status: r.State.Status, ExitCode: r.State.ExitCode}
			}
		}
		return EngineState{}
	}
	s := MenuState{Docker: engine(recipes.Docker), Kube: engine(recipes.Kubernetes), DockerLink: docker, KubeLink: kube}
	if s.Docker.Status != sandbox.Running {
		return s
	}
	s.ListErr = containers.Err != ""
	s.Listed = containers.Loaded && !s.ListErr
	if !s.Listed {
		return s
	}
	list := slices.Clone(containers.Items)
	slices.SortStableFunc(list, func(a, b dockerapi.Container) int {
		switch ar, br := a.State == "running", b.State == "running"; {
		case ar && !br:
			return -1
		case br && !ar:
			return 1
		}
		return 0
	})
	if len(list) > maxMenuContainers {
		s.More = len(list) - maxMenuContainers
		list = list[:maxMenuContainers]
	}
	s.Containers = list
	return s
}

// RecipeMenu is the menu of a recipe's engine, in the menu bar and the
// tray: its state and the actions on it, the context of its command-line
// tool, and for the Docker Engine its containers. show brings the window
// up, for what opens in it. Build it again when Menus changes.
func (m *Model) RecipeMenu(recipe string, show func()) []ggui.MenuItem {
	s := ggui.Peek(m.Menus)
	e, v, l, title, page := s.Docker, s.DockerLink, m.DockerLink, "Docker Engine", PageDocker
	if recipe == recipes.Kubernetes {
		e, v, l, title, page = s.Kube, s.KubeLink, m.KubeLink, "Kubernetes", PageKubernetes
	}
	id := e.ID
	var items []ggui.MenuItem
	switch {
	case id == "":
		items = append(items,
			off(tr("%s is not set up", title)),
			ggui.MenuAction(tr("Set Up and Start %s", title), "", func() { m.CreateFromRecipe(recipe) }),
		)
	case e.Status.Active():
		items = append(items,
			off(title+" · "+statusText(sandbox.State{Status: e.Status, ExitCode: e.ExitCode})),
			ggui.MenuAction(tr("Stop %s", title), "", func() { m.StopSandbox(id) }).BindEnabled(ggui.Const(e.Status != sandbox.Stopping)),
			ggui.MenuAction(tr("Restart %s", title), "", func() { m.RestartSandbox(id) }).BindEnabled(ggui.Const(e.Status == sandbox.Running)),
		)
	default:
		items = append(items,
			off(title+" · "+statusText(sandbox.State{Status: e.Status, ExitCode: e.ExitCode})),
			ggui.MenuAction(tr("Start %s", title), "", func() { m.StartSandbox(id) }),
		)
	}
	items = append(items, ggui.MenuAction(tr("Open %s Page", title), "", func() { show(); m.Go(page) }))
	if label, run := linkMenuAction(m, l, v); run != nil {
		items = append(items, ggui.MenuSeparator(), ggui.MenuAction(label, "", run).BindEnabled(ggui.Const(!v.C.Busy)))
	}
	if recipe == recipes.Docker && e.Status == sandbox.Running {
		items = append(items, containerItems(m, s, show)...)
	}
	return items
}

// off is a line of a menu that tells and does nothing.
func off(label string) ggui.MenuItem {
	return ggui.MenuAction(label, "", nil).BindEnabled(ggui.Const(false))
}

// linkMenuAction is the one action a menu offers on a tool's context, or
// a nil run for none.
func linkMenuAction(m *Model, l *Link, v LinkView) (string, func()) {
	switch v.Status {
	case ContextMissing:
		return tr("Set Up the %s Context for %s", l.Name, l.Tool), func() { m.SetUpLink(l) }
	case ContextStale:
		return tr("Update the %s Context", l.Name), func() { m.UpdateLink(l) }
	case ContextElsewhere:
		return tr("Switch %s to the %s Context", l.Tool, l.Name), func() { m.UseLink(l) }
	case ContextInUse:
		if v.Back == "" {
			return tr("Stop Using the %s Context", l.Name), func() { m.SwitchLinkBack(l) }
		}
		return tr("Switch %s Back to %s", l.Tool, v.Back), func() { m.SwitchLinkBack(l) }
	}
	return "", nil
}

// containerItems are the Docker menu's containers, each a submenu of what
// can be done to it.
func containerItems(m *Model, s MenuState, show func()) []ggui.MenuItem {
	items := []ggui.MenuItem{ggui.MenuSeparator()}
	switch {
	case s.ListErr:
		return append(items, off(tr("Could not reach dockerd")))
	case !s.Listed:
		return nil
	case len(s.Containers) == 0:
		return append(items, off(tr("No containers")))
	}
	items = append(items, off(tr("Containers")))
	for _, c := range s.Containers {
		label := "○ " + c.Name()
		var acts []ggui.MenuItem
		if c.State == "running" {
			label = "● " + c.Name()
			acts = append(acts,
				ggui.MenuAction(tr("Stop"), "", func() { m.DockerAction("stop", c) }),
				ggui.MenuAction(tr("Restart"), "", func() { m.DockerAction("restart", c) }),
			)
		} else {
			acts = append(acts, ggui.MenuAction(tr("Start"), "", func() { m.DockerAction("start", c) }))
		}
		acts = append(acts,
			ggui.MenuAction(tr("Show Logs"), "", func() { show(); m.ShowContainerLogs(c.ID) }),
			ggui.MenuAction(tr("Copy docker logs command"), "", func() { m.Copy("docker logs -f "+c.Name(), "docker logs") }),
			ggui.MenuSeparator(),
			ggui.MenuAction(tr("Remove…"), "", func() { show(); m.AskRemoveContainer(c) }),
		)
		items = append(items, ggui.Menu(label, acts...))
	}
	if s.More > 0 {
		items = append(items, ggui.MenuAction(tr("All %d Containers…", len(s.Containers)+s.More), "", func() { show(); m.Go(PageContainers) }))
	}
	return items
}
