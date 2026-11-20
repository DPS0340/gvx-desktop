package ui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// ---- Images -------------------------------------------------------------

// LoadImages reads the image cache.
func (m *Model) LoadImages() {
	c := m.Client()
	if c.Path == "" {
		return
	}
	m.Images.Update(func(l Listing[rungv.Image]) Listing[rungv.Image] { l.Loading = true; return l })
	var items []rungv.Image
	m.async(func(ctx context.Context) error {
		var err error
		items, err = c.Images(ctx)
		return err
	}, func(err error) {
		l := Listing[rungv.Image]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.Images.Set(l)
	})
}

// OpenPull shows the pull dialog targeting gvx's image cache, with ref filled in.
func (m *Model) OpenPull(ref string) {
	if ggui.Peek(m.Pulling) {
		m.PullOpen.Set(true)
		return
	}
	m.PullTarget.Set(PullTargetGVX)
	m.PullRef.Set(ref)
	m.PullLog.Set("")
	m.PullOpen.Set(true)
}

// Pull downloads the image named in the pull dialog into gvx's cache or the
// Docker Engine, according to PullTarget.
func (m *Model) Pull() {
	ref := strings.TrimSpace(ggui.Peek(m.PullRef))
	if ref == "" || ggui.Peek(m.Pulling) {
		return
	}
	target := ggui.Peek(m.PullTarget)
	m.Pulling.Set(true)
	if target == PullTargetDocker {
		m.PullLog.Set("$ docker pull " + ref + "\n")
	} else {
		m.PullLog.Set("$ rungv pull " + ref + "\n")
	}
	var mu sync.Mutex
	var pending []string
	flush := func() {
		mu.Lock()
		lines := pending
		pending = nil
		mu.Unlock()
		if len(lines) > 0 {
			m.PullLog.Update(func(s string) string {
				s += strings.Join(lines, "\n") + "\n"
				if len(s) > 32*1024 {
					s = s[len(s)-32*1024:]
				}
				return s
			})
		}
	}
	onLine := func(line string) {
		mu.Lock()
		pending = append(pending, line)
		mu.Unlock()
		m.post(flush)
	}
	if target == PullTargetDocker {
		host := m.dockerHost()
		m.async(func(ctx context.Context) error {
			c, err := dockerapi.New(host)
			if err != nil {
				return err
			}
			defer c.Close()
			return c.PullImage(ctx, ref, onLine)
		}, func(err error) {
			flush()
			m.Pulling.Set(false)
			if err != nil {
				m.PullLog.Update(func(s string) string { return s + "error: " + err.Error() + "\n" })
				m.toastError(tr("Pull failed"), err)
				return
			}
			m.PullOpen.Set(false)
			m.toastOK(tr("Pulled %s", ref), tr("The image is ready in the Docker Engine."))
			m.LoadDockerImages()
		})
		return
	}
	c := m.Client()
	m.async(func(ctx context.Context) error {
		return c.Pull(ctx, ref, onLine)
	}, func(err error) {
		flush()
		m.Pulling.Set(false)
		if err != nil {
			m.PullLog.Update(func(s string) string { return s + "error: " + err.Error() + "\n" })
			m.toastError(tr("Pull failed"), err)
			return
		}
		m.PullOpen.Set(false)
		m.toastOK(tr("Pulled %s", ref), tr("The image layers are compiled to EROFS and cached."))
		m.LoadImages()
		m.LoadDisk()
	})
}

// InspectImage opens the image explorer on the Inspect tab for a gvx image.
func (m *Model) InspectImage(ref string) {
	for _, img := range ggui.Peek(m.Images).Items {
		if img.Ref() == ref {
			m.ExploreGVXImage(img, ExploreInspect)
			return
		}
	}
	m.ExploreGVXImage(rungv.Image{ID: ref, Repository: ref}, ExploreInspect)
}

// AskRemoveImage confirms and removes an image.
func (m *Model) AskRemoveImage(img rungv.Image) {
	ref := img.Ref()
	m.Confirm(tr("Remove %s?", ref), tr("The image record is removed and layers no other image uses are deleted (%s).", img.Size), tr("Remove image"), func() {
		c := m.Client()
		m.async(func(ctx context.Context) error { return c.RemoveImage(ctx, ref, true) }, func(err error) {
			if err != nil {
				m.toastError(tr("Could not remove %s", ref), err)
			} else {
				m.toastOK(tr("Removed %s", ref), "")
			}
			m.LoadImages()
			m.LoadDisk()
		})
	})
}

// AskPruneImages removes dangling images.
func (m *Model) AskPruneImages() {
	m.Confirm(tr("Remove unused images?"), tr("Dangling images and orphaned layer files are deleted."), tr("Prune images"), func() {
		c := m.Client()
		var out string
		m.async(func(ctx context.Context) error {
			var err error
			out, err = c.PruneImages(ctx, false)
			return err
		}, func(err error) {
			if err != nil {
				m.toastError(tr("Prune failed"), err)
			} else {
				m.toastOK(tr("Images pruned"), lastLine(out))
			}
			m.LoadImages()
			m.LoadDisk()
		})
	})
}

// ---- Volumes ------------------------------------------------------------

// LoadVolumes reads the local volumes.
func (m *Model) LoadVolumes() {
	c := m.Client()
	if c.Path == "" {
		return
	}
	m.Volumes.Update(func(l Listing[rungv.Volume]) Listing[rungv.Volume] { l.Loading = true; return l })
	var items []rungv.Volume
	m.async(func(ctx context.Context) error {
		var err error
		items, err = c.Volumes(ctx)
		return err
	}, func(err error) {
		l := Listing[rungv.Volume]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.Volumes.Set(l)
	})
}

// VolumeUsers names the sandboxes that mount a volume.
func VolumeUsers(rows []Row, name string) []string {
	var out []string
	for _, r := range rows {
		if slices.Contains(r.Spec.Volumes(), name) {
			out = append(out, r.Spec.Name)
		}
	}
	return out
}

// CreateVolume formats the volume named in the dialog.
func (m *Model) CreateVolume() {
	name := strings.TrimSpace(ggui.Peek(m.VolumeName))
	driver := ggui.Peek(m.VolumeDriver)
	c := m.Client()
	m.async(func(ctx context.Context) error { return c.CreateVolume(ctx, name, driver) }, func(err error) {
		if err != nil {
			m.toastError(tr("Could not create the volume"), err)
			return
		}
		m.VolumeOpen.Set(false)
		m.VolumeName.Set("")
		m.toastOK(tr("Volume created"), tr("Mount it in a sandbox as %s:/path.", name))
		m.LoadVolumes()
	})
}

// AskRemoveVolume confirms and deletes a volume.
func (m *Model) AskRemoveVolume(v rungv.Volume) {
	users := VolumeUsers(ggui.Peek(m.Rows), v.Name)
	text := tr("All data in %s (%s) is deleted. This cannot be undone.", v.Name, v.Size)
	if len(users) > 0 {
		text += tr(" Used by: %s.", strings.Join(users, ", "))
	}
	m.Confirm(tr("Delete volume %s?", v.Name), text, tr("Delete volume"), func() {
		c := m.Client()
		m.async(func(ctx context.Context) error { return c.RemoveVolume(ctx, v.Name) }, func(err error) {
			if err != nil {
				m.toastError(tr("Could not delete %s", v.Name), err)
			} else {
				m.toastOK(tr("Deleted %s", v.Name), "")
			}
			m.LoadVolumes()
			m.LoadDisk()
		})
	})
}

// AskPruneVolumes deletes every unlocked volume.
func (m *Model) AskPruneVolumes() {
	m.Confirm(tr("Delete all unused volumes?"), tr("Every volume no running sandbox holds is deleted, with its data. Saved sandboxes that mount them start empty."), tr("Prune volumes"), func() {
		c := m.Client()
		var out string
		m.async(func(ctx context.Context) error {
			var err error
			out, err = c.PruneVolumes(ctx)
			return err
		}, func(err error) {
			if err != nil {
				m.toastError(tr("Prune failed"), err)
			} else {
				m.toastOK(tr("Volumes pruned"), lastLine(out))
			}
			m.LoadVolumes()
			m.LoadDisk()
		})
	})
}

// LoadDisk reads the disk usage totals.
func (m *Model) LoadDisk() {
	c := m.Client()
	if c.Path == "" {
		return
	}
	var rows []rungv.DiskUsage
	m.async(func(ctx context.Context) error {
		var err error
		rows, err = c.DiskUsage(ctx)
		return err
	}, func(err error) {
		if err == nil {
			m.Disk.Set(rows)
		}
	})
}

// ---- Docker and Kubernetes ----------------------------------------------

func (m *Model) dockerHost() string { return m.DockerAPI().Host() }

// Docker endpoints, as the settings name them.
const (
	DockerSocketEndpoint = "socket"
	DockerTCPEndpoint    = "tcp"
)

// dockerSetting is how the settings ask the Docker Engine recipe to
// publish its API: a new engine is made so.
func (m *Model) dockerSetting() recipes.DockerAPI { return dockerSettingOf(ggui.Peek(m.Settings)) }

func dockerSettingOf(set store.Settings) recipes.DockerAPI {
	api := recipes.DefaultDockerAPI()
	switch set.DockerEndpoint {
	case DockerSocketEndpoint:
		api.TCP = false
	case DockerTCPEndpoint:
		api.TCP = true
	}
	if set.DockerPort > 0 {
		api.Port = set.DockerPort
	}
	return api
}

// DockerAPI is how the Docker Engine is reached: as its sandbox publishes
// the API when there is one, whose ports are its own, and else as the
// settings would make one.
func (m *Model) DockerAPI() recipes.DockerAPI {
	return m.dockerAPIOf(ggui.Peek(m.Rows), ggui.Peek(m.Settings))
}

func (m *Model) dockerAPIOf(rows []Row, set store.Settings) recipes.DockerAPI {
	if row, ok := m.RecipeSandbox(rows, recipes.Docker); ok {
		if api, ok := recipes.DockerAPIOf(row.Spec); ok {
			return api
		}
	}
	return dockerSettingOf(set)
}

// DockerHost is the DOCKER_HOST value that reaches the Docker Engine.
func (m *Model) DockerHost() string { return m.dockerHost() }

// SetDockerAPI changes how the engine's API is published, in the settings
// and in the engine's sandbox when there is one. A sandbox's ports are
// fixed when it starts, so a running engine keeps its endpoint until it is
// restarted, and is told so.
func (m *Model) SetDockerAPI(api recipes.DockerAPI) {
	if api.Port <= 0 || api.Port > 65535 {
		api.Port = recipes.DefaultDockerPort
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.DockerEndpoint = DockerSocketEndpoint
		if api.TCP {
			s.DockerEndpoint = DockerTCPEndpoint
		}
		s.DockerPort = api.Port
	})
	specs := slices.Clone(ggui.Peek(m.Specs))
	for i, spec := range specs {
		if spec.Recipe != recipes.Docker {
			continue
		}
		if was, ok := recipes.DockerAPIOf(spec); ok && was.Host() == api.Host() {
			return
		}
		specs[i] = recipes.WithDockerAPI(spec, api)
		m.saveSpecs(specs)
		if m.sup.State(spec.ID).Status.Active() {
			m.Toasts.Push(ui.Toast(tr("Restart the Docker Engine to move its API"),
				tr("It keeps answering on its old endpoint until then; after the restart it is %s.", api.Host())).Duration(8 * time.Second))
		}
		m.LoadDocker()
		return
	}
}

// LoadDocker lists the containers of the Docker Engine recipe. Asked for
// while a listing runs, it lists them again after it.
func (m *Model) LoadDocker() {
	if m.Offline {
		return // the containers shown are made up
	}
	row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	if !ok || row.State.Status != sandbox.Running {
		m.DockerContainers.Set(Listing[dockerapi.Container]{})
		return
	}
	if m.dockerLoading {
		m.dockerAgain = true
		return
	}
	m.dockerLoading = true
	host := m.dockerHost()
	var items []dockerapi.Container
	var version dockerapi.Version
	m.DockerContainers.Update(func(l Listing[dockerapi.Container]) Listing[dockerapi.Container] { l.Loading = true; return l })
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if version, err = c.Version(ctx); err != nil {
			return err
		}
		items, err = c.Containers(ctx, true)
		return err
	}, func(err error) {
		m.dockerLoading = false
		if m.dockerAgain {
			m.dockerAgain = false
			m.LoadDocker()
			return
		}
		if row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker); !ok || row.State.Status != sandbox.Running {
			return // the engine stopped meanwhile, and its list was emptied
		}
		l := Listing[dockerapi.Container]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		} else {
			m.DockerVersion.Set(version)
		}
		m.DockerContainers.Set(l)
	})
}

// followDocker keeps the engine's containers current while it runs, for
// the Docker and Containers pages and the menus: often while a page shows
// them, now and then otherwise, and not at all while the engine is stopped.
// The logs of the container the Containers page shows follow with them.
// Call it from Start.
func (m *Model) followDocker() {
	if m.Offline {
		return
	}
	every := ggui.Derived(func() time.Duration { return m.recipePageInterval(recipes.Docker, PageDocker, PageContainers) })
	logs := ggui.Derived(m.followsContainerLogs)
	m.poll(every, m.LoadDocker, func() {
		m.LoadDocker()
		if ggui.Peek(logs) {
			m.LoadContainerLogs()
		}
	})
	imagesEvery := ggui.Derived(func() time.Duration {
		row, ok := m.RecipeSandbox(m.Rows.Get(), recipes.Docker)
		if !ok || row.State.Status != sandbox.Running {
			return 0
		}
		switch m.Page.Get() {
		case PageImages:
			if m.ImageView.Get() == ImagesDocker {
				return 3 * time.Second
			}
			return 0
		case PageDocker:
			return 10 * time.Second
		}
		return 0
	})
	m.poll(imagesEvery, m.LoadDockerImages, m.LoadDockerImages)
	id := ggui.Map(m.Container, func(c dockerapi.Container) string { return c.ID })
	shown := ggui.Combine(logs, id, func(on bool, id string) string {
		if !on {
			return ""
		}
		return id
	})
	m.followLogs(shown, id, m.LoadContainerLogs, m.ContainerLogs)
}

// followLinks asks docker and kubectl about their contexts at the start,
// whenever a screen that tells of them opens, and when what they should
// point at changes. Call it from Start.
func (m *Model) followLinks() {
	if m.Offline {
		return
	}
	shown := func(page, recipe string) *ggui.DerivedValue[bool] {
		return ggui.Derived(func() bool {
			switch m.Page.Get() {
			case page:
				return true
			case PageSandboxes:
				if m.Detail.Get() != connectTab {
					return false
				}
				id := m.Selected.Get()
				for _, s := range m.Specs.Get() {
					if s.ID == id {
						return s.Recipe == recipe
					}
				}
			}
			return false
		})
	}
	ggui.Watch(shown(PageDocker, recipes.Docker), func(on bool) {
		if on {
			m.CheckLink(m.DockerLink)
		}
	})
	ggui.Watch(shown(PageKubernetes, recipes.Kubernetes), func(on bool) {
		if on {
			m.CheckLink(m.KubeLink)
		}
	})
	// Each runs at once as well, which asks both tools at the start.
	ggui.Watch(m.DockerEndpoint, func(string) { m.CheckLink(m.DockerLink) })
	ggui.Watch(m.KubeconfigReady, func(bool) { m.CheckLink(m.KubeLink) })
}

// DockerAction runs start, stop, restart or remove on a container.
func (m *Model) DockerAction(action string, ctr dockerapi.Container) {
	host := m.dockerHost()
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return dockerDo(ctx, c, action, ctr.ID)
	}, func(err error) {
		if err != nil {
			m.toastError(fmt.Sprintf(tr("Could not %s %s"), action, ctr.Name()), err)
		}
		m.LoadDocker()
		if ggui.Peek(m.Page) == PageContainers && ggui.Peek(m.Container).ID == ctr.ID {
			m.LoadContainerLogs() // what it wrote as it stopped or started
		}
	})
}

// dockerDo runs start, stop, restart or remove on the container id.
func dockerDo(ctx context.Context, c *dockerapi.Client, action, id string) error {
	switch action {
	case "start":
		return c.Start(ctx, id)
	case "stop":
		return c.Stop(ctx, id)
	case "restart":
		return c.Restart(ctx, id)
	case "remove":
		return c.Remove(ctx, id, true)
	}
	return fmt.Errorf("unknown action %s", action)
}

// AskRemoveContainer confirms and removes a container.
func (m *Model) AskRemoveContainer(ctr dockerapi.Container) {
	m.Confirm(tr("Remove %s?", ctr.Name()), tr("The container is stopped and removed from the engine."), tr("Remove container"), func() { m.DockerAction("remove", ctr) })
}

// DockerCLI finds the docker command: the one the settings name, and
// otherwise where a terminal would find it.
func (m *Model) DockerCLI() (string, bool) {
	return findCLI("docker", ggui.Peek(m.Settings).DockerPath)
}

// CheckKubeconfig looks for the kubeconfig the k3s recipe writes.
func (m *Model) CheckKubeconfig() {
	_, err := os.Stat(recipes.Kubeconfig())
	m.KubeconfigReady.Set(err == nil)
}

// ---- Settings -----------------------------------------------------------

// UpdateSettings applies and stores a change of the settings.
func (m *Model) UpdateSettings(fn func(*store.Settings)) {
	s := ggui.Peek(m.Settings)
	before := s
	fn(&s)
	m.Settings.Set(s)
	if err := m.store.SaveSettings(s); err != nil {
		m.toastError(tr("Could not save settings"), err)
	}
	if s.RungvPath != before.RungvPath || s.CacheDir != before.CacheDir {
		m.DetectEngine(nil)
	}
	if s.DockerPath != before.DockerPath {
		m.CheckLink(m.DockerLink)
	}
}

// Copy puts text on the clipboard and says so.
func (m *Model) Copy(text, what string) {
	if m.host == nil {
		return
	}
	m.host.Clipboard().Write(text)
	m.Toasts.Push(ui.Toast(tr("Copied"), what).Duration(2 * time.Second))
}

// Open opens a URL or reveals a path.
func (m *Model) Open(target string) {
	var err error
	if strings.Contains(target, "://") {
		err = runtime.OpenURL(target)
	} else {
		err = runtime.Reveal(target)
	}
	if err != nil {
		m.toastError(tr("Could not open %s", target), err)
	}
}

// PrepareQuit decides what quitting does with running sandboxes: true
// when the app may close now.
func (m *Model) PrepareQuit() bool {
	if len(m.sup.Running()) == 0 {
		return true
	}
	m.QuitOpen.Set(true)
	return false
}

// StopAll stops every sandbox, as the user asked: the recipe sandboxes
// among them are not started again with the app.
func (m *Model) StopAll() {
	for _, id := range m.sup.Running() {
		m.setResume(id, false)
	}
	m.stopAll(func() {})
}

// StopAllAndThen stops every sandbox to quit, then runs then on the UI
// thread. The recipe sandboxes running start again with the app.
func (m *Model) StopAllAndThen(then func()) {
	m.quitting = true
	m.stopAll(then)
}

func (m *Model) stopAll(then func()) {
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	id := m.Toasts.Push(ui.Toast(tr("Stopping sandboxes"), tr("Waiting for the guests to exit…")).Loading())
	m.async(func(context.Context) error {
		m.sup.StopAll(timeout)
		return nil
	}, func(error) {
		m.Toasts.Dismiss(id)
		then()
	})
}

// ---- Editor draft -------------------------------------------------------

// Draft is the editor's form: lists are edited as text, one entry a line.
type Draft struct {
	ID, Name, Image, Command string
	Entrypoint               string
	SetEntrypoint            bool
	Ports, Env               string
	Volumes, Binds, Tmpfs    string
	Workspace                string
	WorkspaceRO              bool
	RootfsVolume             string
	Memory, CPUs             string
	Network, WGConf          string
	IsolateLAN               bool
	Backend                  string
	Hostname, User, Workdir  string
	Privileged, HostCACerts  bool
	Pull                     string
	Checkpoint, AutoStart    bool
	ReadyPattern, ExtraArgs  string
	Recipe                   string
	CreatedAt                time.Time
}

// DraftFrom fills the form from a spec.
func DraftFrom(s sandbox.Spec) Draft {
	d := Draft{
		ID: s.ID, Name: s.Name, Image: s.Image, Command: s.Command,
		Entrypoint: s.Entrypoint, SetEntrypoint: s.SetEntrypoint,
		Ports: strings.Join(s.Ports, "\n"), Env: strings.Join(s.Env, "\n"),
		Workspace: s.Workspace, WorkspaceRO: s.WorkspaceRO, RootfsVolume: s.RootfsVolume,
		Memory: s.Memory, CPUs: s.CPUs, Network: s.Network, WGConf: s.WGConf, IsolateLAN: s.IsolateLAN,
		Backend: s.Backend, Hostname: s.Hostname, User: s.User, Workdir: s.Workdir,
		Privileged: !s.Unprivileged, HostCACerts: s.HostCACerts, Pull: s.Pull,
		Checkpoint: s.Checkpoint, AutoStart: s.AutoStart, ReadyPattern: s.ReadyPattern,
		ExtraArgs: s.ExtraArgs, Recipe: s.Recipe, CreatedAt: s.CreatedAt,
	}
	if d.Network == "" {
		d.Network = "nat"
	}
	if d.Backend == "" {
		d.Backend = "auto"
	}
	if d.Pull == "" {
		d.Pull = "missing"
	}
	var vols, binds, tmpfs []string
	for _, mt := range s.Mounts {
		switch mt.Type {
		case sandbox.MountVolume:
			vols = append(vols, mountLine(mt.Source, mt.Target, mt.ReadOnly))
		case sandbox.MountBind:
			binds = append(binds, mountLine(mt.Source, mt.Target, mt.ReadOnly))
		case sandbox.MountTmpfs:
			line := mt.Target
			var opts []string
			if mt.Size != "" {
				opts = append(opts, "size="+mt.Size)
			}
			if mt.ReadOnly {
				opts = append(opts, "ro")
			}
			if len(opts) > 0 {
				line += ":" + strings.Join(opts, ",")
			}
			tmpfs = append(tmpfs, line)
		}
	}
	d.Volumes, d.Binds, d.Tmpfs = strings.Join(vols, "\n"), strings.Join(binds, "\n"), strings.Join(tmpfs, "\n")
	return d
}

func mountLine(src, dst string, ro bool) string {
	l := src + ":" + dst
	if ro {
		l += ":ro"
	}
	return l
}

func lines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// parseMount splits SRC:/DST[:ro|rw], keeping colons in SRC such as a
// Windows drive letter.
func parseMount(line string) (src, dst string, ro bool, err error) {
	if rest, ok := strings.CutSuffix(line, ":ro"); ok {
		line, ro = rest, true
	} else if rest, ok := strings.CutSuffix(line, ":rw"); ok {
		line = rest
	}
	i := strings.LastIndex(line, ":/")
	if i <= 0 {
		return "", "", false, fmt.Errorf("mount %q: write SOURCE:/guest/path[:ro]", line)
	}
	return line[:i], line[i+1:], ro, nil
}

// Spec converts the form to a sandbox spec.
func (d Draft) Spec() (sandbox.Spec, error) {
	s := sandbox.Spec{
		ID: d.ID, Name: strings.TrimSpace(d.Name), Image: strings.TrimSpace(d.Image),
		Command: strings.TrimSpace(d.Command), Entrypoint: strings.TrimSpace(d.Entrypoint), SetEntrypoint: d.SetEntrypoint,
		Ports: lines(d.Ports), Env: lines(d.Env),
		Workspace: strings.TrimSpace(d.Workspace), WorkspaceRO: d.WorkspaceRO, RootfsVolume: strings.TrimSpace(d.RootfsVolume),
		Memory: strings.TrimSpace(d.Memory), CPUs: strings.TrimSpace(d.CPUs), WGConf: strings.TrimSpace(d.WGConf), IsolateLAN: d.IsolateLAN,
		Hostname: strings.TrimSpace(d.Hostname), User: strings.TrimSpace(d.User), Workdir: strings.TrimSpace(d.Workdir),
		Unprivileged: !d.Privileged, HostCACerts: d.HostCACerts,
		Checkpoint: d.Checkpoint, AutoStart: d.AutoStart, ReadyPattern: strings.TrimSpace(d.ReadyPattern),
		ExtraArgs: strings.TrimSpace(d.ExtraArgs), Recipe: d.Recipe, CreatedAt: d.CreatedAt,
	}
	if d.Network != "nat" {
		s.Network = d.Network
	}
	if d.Backend != "auto" {
		s.Backend = d.Backend
	}
	if d.Pull != "missing" {
		s.Pull = d.Pull
	}
	for _, l := range lines(d.Volumes) {
		src, dst, ro, err := parseMount(l)
		if err != nil {
			return s, err
		}
		s.Mounts = append(s.Mounts, sandbox.Mount{Type: sandbox.MountVolume, Source: src, Target: dst, ReadOnly: ro})
	}
	for _, l := range lines(d.Binds) {
		src, dst, ro, err := parseMount(l)
		if err != nil {
			return s, err
		}
		s.Mounts = append(s.Mounts, sandbox.Mount{Type: sandbox.MountBind, Source: src, Target: dst, ReadOnly: ro})
	}
	for _, l := range lines(d.Tmpfs) {
		target, opts, _ := strings.Cut(l, ":")
		mt := sandbox.Mount{Type: sandbox.MountTmpfs, Target: target}
		for o := range strings.SplitSeq(opts, ",") {
			switch {
			case strings.HasPrefix(o, "size="):
				mt.Size = strings.TrimPrefix(o, "size=")
			case o == "ro":
				mt.ReadOnly = true
			}
		}
		s.Mounts = append(s.Mounts, mt)
	}
	return s, nil
}

// ---- Helpers ------------------------------------------------------------

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func removeAll(dir string) error { return os.RemoveAll(dir) }

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return tr("just now")
	case d < time.Minute:
		return fmt.Sprintf(tr("%ds ago"), int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf(tr("%dm ago"), int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(tr("%dh ago"), int(d.Hours()))
	}
	return t.Format(tr("Jan 2 15:04"))
}

func uptime(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf(tr("%ds"), int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf(tr("%dm %ds"), int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf(tr("%dh %dm"), int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf(tr("%dd %dh"), int(d.Hours())/24, int(d.Hours())%24)
}
