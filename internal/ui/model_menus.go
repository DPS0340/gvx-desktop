package ui

import (
	"slices"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// maxMenuContainers is how many containers the Docker menu lists, and
// pods the Kubernetes menu; the Containers and Pods pages list them all.
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
	// Pods are the ones the Kubernetes menu lists, the user's first, and
	// PodsMore the number left out; the others say as Listed and ListErr
	// do, and PodsWaiting that the cluster's API has yet to answer.
	Pods                             []MenuPod
	PodsMore                         int
	PodsListed, PodsErr, PodsWaiting bool
	DockerLink                       LinkView
	KubeLink                         LinkView
}

// MenuPod is a pod as the Kubernetes menu tells of it.
type MenuPod struct {
	Pod    kubeapi.Pod
	Status string
	Health kubeapi.Health
}

// Equal makes MenuState comparable for signals. Containers and pods are
// equal when the menu tells the same of them.
func (s MenuState) Equal(o MenuState) bool {
	return s.Docker == o.Docker && s.Kube == o.Kube && s.More == o.More && s.Listed == o.Listed && s.ListErr == o.ListErr &&
		s.PodsMore == o.PodsMore && s.PodsListed == o.PodsListed && s.PodsErr == o.PodsErr && s.PodsWaiting == o.PodsWaiting &&
		slices.EqualFunc(s.Pods, o.Pods, func(a, b MenuPod) bool {
			return a.Pod.Key() == b.Pod.Key() && a.Status == b.Status && a.Health == b.Health
		}) &&
		s.DockerLink.Equal(o.DockerLink) && s.KubeLink.Equal(o.KubeLink) &&
		slices.EqualFunc(s.Containers, o.Containers, func(a, b dockerapi.Container) bool {
			return a.ID == b.ID && a.Name() == b.Name() && a.State == b.State
		})
}

// menuState sums up for the menus the sandboxes, the Docker Engine's
// containers, the cluster's pods and the contexts.
func menuState(rows []Row, containers Listing[dockerapi.Container], pods Listing[kubeapi.Pod], podsWaiting bool, docker, kube LinkView) MenuState {
	engine := func(recipe string) EngineState {
		for _, r := range rows {
			if r.Spec.Recipe == recipe {
				return EngineState{ID: r.Spec.ID, Status: r.State.Status, ExitCode: r.State.ExitCode}
			}
		}
		return EngineState{}
	}
	s := MenuState{Docker: engine(recipes.Docker), Kube: engine(recipes.Kubernetes), DockerLink: docker, KubeLink: kube}
	if s.Kube.Status == sandbox.Running {
		s.menuPods(pods, podsWaiting)
	}
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
	if recipe == recipes.Kubernetes && e.Status == sandbox.Running {
		items = append(items, podItems(m, s, show)...)
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

// menuPods sums up the cluster's pods for the Kubernetes menu.
func (s *MenuState) menuPods(pods Listing[kubeapi.Pod], waiting bool) {
	s.PodsWaiting = waiting
	s.PodsErr = pods.Err != ""
	s.PodsListed = pods.Loaded && !s.PodsErr
	if !s.PodsListed {
		return
	}
	list := sortPods(pods.Items)
	if len(list) > maxMenuContainers {
		s.PodsMore = len(list) - maxMenuContainers
		list = list[:maxMenuContainers]
	}
	for _, p := range list {
		sum := p.Summarize()
		s.Pods = append(s.Pods, MenuPod{Pod: p, Status: sum.Status, Health: sum.Health()})
	}
}

// podItems are the Kubernetes menu's pods, each a submenu of what can be
// done to it.
func podItems(m *Model, s MenuState, show func()) []ggui.MenuItem {
	items := []ggui.MenuItem{ggui.MenuSeparator()}
	switch {
	case s.PodsWaiting:
		return append(items, off(tr("The cluster is starting…")))
	case s.PodsErr:
		return append(items, off(tr("Could not reach the API server")))
	case !s.PodsListed:
		return nil
	case len(s.Pods) == 0:
		return append(items, off(tr("No pods")))
	}
	items = append(items, off(tr("Pods")))
	link := ggui.Peek(m.KubeLink.View)
	for _, mp := range s.Pods {
		p := mp.Pod
		label := "○ " + p.Name()
		if mp.Health == kubeapi.Healthy {
			label = "● " + p.Name()
		}
		if mp.Status != "Running" {
			label += " · " + mp.Status
		}
		ctr := podLogContainer(p, "")
		items = append(items, ggui.Menu(label,
			ggui.MenuAction(tr("Show Logs"), "", func() { show(); m.ShowPodLogs(p) }),
			ggui.MenuAction(tr("Copy kubectl logs command"), "", func() { m.Copy(kubectlLogs(link, p, ctr), "kubectl logs") }),
			ggui.MenuSeparator(),
			ggui.MenuAction(tr("Delete…"), "", func() { show(); m.AskDeletePod(p) }),
		))
	}
	if s.PodsMore > 0 {
		items = append(items, ggui.MenuAction(tr("All %d Pods…", len(s.Pods)+s.PodsMore), "", func() { show(); m.Go(PagePods) }))
	}
	return items
}
