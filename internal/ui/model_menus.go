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
	Paused   bool
	Control  bool
}

// MenuState is what the Docker and Kubernetes menus tell. Equal states make
// the same menus, so the menus are built again only when it changes: not
// for a line of a log, nor for a container's uptime.
type MenuState struct {
	Docker, Kube  EngineState
	SandboxEngine bool
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
	// Forwards are the port forwards that run, as the menu tells of them.
	Forwards   []PortForward
	DockerLink LinkView
	KubeLink   LinkView
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
	return s.Docker == o.Docker && s.Kube == o.Kube && s.SandboxEngine == o.SandboxEngine && s.More == o.More && s.Listed == o.Listed && s.ListErr == o.ListErr &&
		s.PodsMore == o.PodsMore && s.PodsListed == o.PodsListed && s.PodsErr == o.PodsErr && s.PodsWaiting == o.PodsWaiting &&
		slices.EqualFunc(s.Pods, o.Pods, func(a, b MenuPod) bool {
			return a.Pod.Key() == b.Pod.Key() && a.Status == b.Status && a.Health == b.Health
		}) &&
		s.DockerLink.Equal(o.DockerLink) && s.KubeLink.Equal(o.KubeLink) && slices.Equal(s.Forwards, o.Forwards) &&
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
				return EngineState{
					ID:       r.Spec.ID,
					Status:   r.State.Status,
					ExitCode: r.State.ExitCode,
					Paused:   r.State.Paused,
					Control:  r.State.Control,
				}
			}
		}
		return EngineState{}
	}
	s := MenuState{Docker: engine(recipes.Docker), Kube: engine(recipes.Kubernetes), DockerLink: docker, KubeLink: kube}
	if s.Kube.Status == sandbox.Running && !s.Kube.Paused {
		s.menuPods(pods, podsWaiting)
	}
	if s.Docker.Status == sandbox.Running && !s.Docker.Paused {
		s.menuContainers(containers)
	}
	return s
}

func (s *MenuState) menuContainers(containers Listing[dockerapi.Container]) {
	s.ListErr = containers.Err != ""
	s.Listed = containers.Loaded && !s.ListErr
	if !s.Listed {
		return
	}
	list := slices.Clone(containers.Items)
	rank := func(state string) int {
		switch state {
		case "running":
			return 0
		case "paused":
			return 1
		}
		return 2
	}
	slices.SortStableFunc(list, func(a, b dockerapi.Container) int {
		return rank(a.State) - rank(b.State)
	})
	if len(list) > maxMenuContainers {
		s.More = len(list) - maxMenuContainers
		list = list[:maxMenuContainers]
	}
	s.Containers = list
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
	if recipe == recipes.Docker && s.SandboxEngine {
		items = append(items, off(tr("Docker · Sandbox Engine")))
	} else {
		switch {
		case id == "":
			items = append(items,
				off(tr("%s is not set up", title)),
				ggui.MenuAction(tr("Set Up and Start %s", title), "", func() { m.CreateFromRecipe(recipe) }),
			)
		case e.Status.Active() && e.Paused:
			items = append(items,
				off(title+" · "+statusText(sandbox.State{Status: e.Status, ExitCode: e.ExitCode, Paused: true})),
				ggui.MenuAction(tr("Resume %s", title), "", func() { m.ResumeSandbox(id) }),
				ggui.MenuAction(tr("Stop %s", title), "", func() { m.StopSandbox(id) }),
			)
		case e.Status.Active():
			items = append(items,
				off(title+" · "+statusText(sandbox.State{Status: e.Status, ExitCode: e.ExitCode})),
			)
			if e.Control && e.Status != sandbox.Stopping {
				items = append(items, ggui.MenuAction(tr("Pause %s", title), "", func() { m.PauseSandbox(id) }))
			}
			items = append(items,
				ggui.MenuAction(tr("Stop %s", title), "", func() { m.StopSandbox(id) }).BindEnabled(ggui.Const(e.Status != sandbox.Stopping)),
				ggui.MenuAction(tr("Restart %s", title), "", func() { m.RestartSandbox(id) }).BindEnabled(ggui.Const(e.Status == sandbox.Running)),
			)
		default:
			items = append(items,
				off(title+" · "+statusText(sandbox.State{Status: e.Status, ExitCode: e.ExitCode})),
				ggui.MenuAction(tr("Start %s", title), "", func() { m.StartSandbox(id) }),
			)
		}
	}
	items = append(items, ggui.MenuAction(tr("Open %s Page", title), "", func() { show(); m.Go(page) }))
	if label, run := linkMenuAction(m, l, v); run != nil {
		items = append(items, ggui.MenuSeparator(), ggui.MenuAction(label, "", run).BindEnabled(ggui.Const(!v.C.Busy)))
	}
	if recipe == recipes.Docker && (s.SandboxEngine || (e.Status == sandbox.Running && !e.Paused)) {
		items = append(items, containerItems(m, s, show)...)
	}
	if recipe == recipes.Kubernetes && e.Status == sandbox.Running && !e.Paused {
		items = append(items, podItems(m, s, show)...)
		items = append(items, forwardItems(m, s)...)
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
		if s.SandboxEngine {
			return append(items, off(tr("Could not reach the sandbox engine")))
		}
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
		switch c.State {
		case "running":
			label = "● " + c.Name()
			if !s.SandboxEngine || m.canControlContainer(c.ID) {
				acts = append(acts, ggui.MenuAction(tr("Pause"), "", func() { m.DockerAction("pause", c) }))
			}
			acts = append(acts,
				ggui.MenuAction(tr("Stop"), "", func() { m.DockerAction("stop", c) }),
				ggui.MenuAction(tr("Restart"), "", func() { m.DockerAction("restart", c) }),
			)
		case "paused":
			label = "● " + c.Name() + " · " + tr("Paused")
			if !s.SandboxEngine || m.canControlContainer(c.ID) {
				acts = append(acts, ggui.MenuAction(tr("Resume"), "", func() { m.DockerAction("unpause", c) }))
			}
			acts = append(acts, ggui.MenuAction(tr("Stop"), "", func() { m.DockerAction("stop", c) }))
		default:
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

// menuForwards are the forwards as the menu tells of them: where they
// listen and what they reach, not how their connections went, which would
// build the menu again for each.
func menuForwards(list []PortForward) []PortForward {
	out := make([]PortForward, len(list))
	for i, f := range list {
		out[i] = PortForward{ID: f.ID, Target: f.Target, Local: f.Local}
	}
	return out
}

// forwardItems are the Kubernetes menu's port forwards, each a submenu to
// open, copy or stop it.
func forwardItems(m *Model, s MenuState) []ggui.MenuItem {
	if len(s.Forwards) == 0 {
		return nil
	}
	items := []ggui.MenuItem{ggui.MenuSeparator(), off(tr("Port Forwards"))}
	for _, f := range s.Forwards {
		items = append(items, ggui.Menu(f.Address()+" → "+f.TargetText(),
			ggui.MenuAction(tr("Open in Browser"), "", func() { m.Open(f.URL()) }),
			ggui.MenuAction(tr("Copy Address"), "", func() { m.Copy(f.Address(), f.Address()) }),
			ggui.MenuSeparator(),
			ggui.MenuAction(tr("Stop"), "", func() { m.StopForward(f.ID, "") }),
		))
	}
	return items
}
