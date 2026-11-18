// Package ui is the desktop app's interface, built with ggui.
package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// Pages of the sidebar.
const (
	PageDashboard  = "dashboard"
	PageSandboxes  = "sandboxes"
	PageImages     = "images"
	PageVolumes    = "volumes"
	PageSnapshots  = "snapshots"
	PageDocker     = "docker"
	PageKubernetes = "kubernetes"
	PageSettings   = "settings"
)

// Engine is what the app knows of the rungv binary.
type Engine struct {
	Path    string
	Version rungv.Version
	Info    rungv.Info
	Err     string
	Loading bool
	Checked bool
}

// Ready reports a binary that answered and has a backend to run on.
func (e Engine) Ready() bool {
	return e.Path != "" && e.Err == "" && e.Checked && len(e.Info.AvailableBackends) > 0
}

// Equal makes Engine comparable for signals.
func (e Engine) Equal(o Engine) bool {
	return e.Path == o.Path && e.Version == o.Version && e.Err == o.Err && e.Loading == o.Loading &&
		e.Checked == o.Checked && e.Info.DefaultBackend == o.Info.DefaultBackend &&
		slices.Equal(e.Info.AvailableBackends, o.Info.AvailableBackends) &&
		slices.Equal(e.Info.BackendWarnings, o.Info.BackendWarnings) &&
		e.Info.HostLockdown == o.Info.HostLockdown && e.Info.Images == o.Info.Images &&
		e.Info.Volumes == o.Info.Volumes && e.Info.LayerCacheDir == o.Info.LayerCacheDir
}

// Row is a sandbox with its runtime state, as the tables show it.
type Row struct {
	Spec  sandbox.Spec
	State sandbox.State
}

// Equal makes Row comparable for keyed rows.
func (r Row) Equal(o Row) bool { return r.Spec.Equal(o.Spec) && r.State.Equal(o.State) }

// Listing is a list loaded from rungv with its loading state.
type Listing[T any] struct {
	Items   []T
	Err     string
	Loading bool
	Loaded  bool
}

// Model holds the app's state and actions. Signals belong to the UI
// thread; background work posts its results back through post.
type Model struct {
	store store.Store
	sup   *sandbox.Supervisor
	host  ggui.Host

	clientMu sync.Mutex
	client   *rungv.Client

	Toasts *ui.ToasterWidget

	Page      *ggui.StateValue[string]
	Settings  *ggui.StateValue[store.Settings]
	Engine    *ggui.StateValue[Engine]
	Update    *ggui.StateValue[Update]
	Autostart *ggui.StateValue[bool]   // registered to start at login
	BuildLog  *ggui.StateValue[string] // the end of what a build from source printed
	release   rungv.Release            // what Update tells of
	Now       *ggui.StateValue[time.Time]
	// Lang is the language the app is worded in, from the settings or the
	// system: LangEnglish or LangKorean.
	Lang *ggui.DerivedValue[string]

	Specs    *ggui.StateValue[[]sandbox.Spec]
	States   *ggui.StateValue[map[string]sandbox.State]
	Rows     *ggui.DerivedValue[[]Row]
	Running  *ggui.DerivedValue[int]
	Selected *ggui.StateValue[string]
	Logs     *ggui.StateValue[[]sandbox.LogLine]
	Detail   *ggui.StateValue[int]
	Query    *ggui.StateValue[string]

	Images    *ggui.StateValue[Listing[rungv.Image]]
	Volumes   *ggui.StateValue[Listing[rungv.Volume]]
	Disk      *ggui.StateValue[[]rungv.DiskUsage]
	Snapshots *ggui.StateValue[[]sandbox.Snapshot]

	DockerContainers *ggui.StateValue[Listing[dockerapi.Container]]
	DockerVersion    *ggui.StateValue[dockerapi.Version]
	KubeconfigReady  *ggui.StateValue[bool]

	// Editor
	EditorOpen *ggui.StateValue[bool]
	Draft      *ggui.StateValue[Draft]
	DraftError *ggui.DerivedValue[string]
	DraftLine  *ggui.DerivedValue[string]

	// Dialogs
	ConfirmOpen   *ggui.StateValue[bool]
	ConfirmTitle  *ggui.StateValue[string]
	ConfirmText   *ggui.StateValue[string]
	ConfirmLabel  *ggui.StateValue[string]
	confirmAction func()
	PullOpen      *ggui.StateValue[bool]
	PullRef       *ggui.StateValue[string]
	PullLog       *ggui.StateValue[string]
	Pulling       *ggui.StateValue[bool]

	// What the pull dialog's search of a registry found; see SearchTyped.
	SearchResults  *ggui.StateValue[[]registry.Result]
	SearchFor      *ggui.StateValue[string] // the words SearchResults answer
	SearchBusy     *ggui.StateValue[bool]
	SearchErr      *ggui.StateValue[string]
	SearchTags     *ggui.StateValue[[]string]
	SearchTagsOf   *ggui.StateValue[string] // the repository SearchTags are of
	SearchTagsBusy *ggui.StateValue[bool]
	search         search
	// Offline keeps the model from asking the network anything, as the
	// screens rendered with made-up state must not.
	Offline        bool
	InspectOpen    *ggui.StateValue[bool]
	InspectTitle   *ggui.StateValue[string]
	InspectBody    *ggui.StateValue[string]
	VolumeOpen     *ggui.StateValue[bool]
	VolumeName     *ggui.StateValue[string]
	VolumeDriver   *ggui.StateValue[string]
	PaletteOpen    *ggui.StateValue[bool]
	PaletteQuery   *ggui.StateValue[string]
	QuitOpen       *ggui.StateValue[bool]
	SidebarNarrow  *ggui.StateValue[bool]
	ImageQuery     *ggui.StateValue[string]
	SelectedImage  *ggui.StateValue[string]
	SelectedVolume *ggui.StateValue[string]

	dirty     atomic.Bool
	scheduled atomic.Bool
	logSeq    int64
	quitting  bool
}

// NewModel loads the saved state. Call it under a ggui owner (ggui.Root).
func NewModel(st store.Store, dataDir string) *Model {
	settings, _ := st.Settings()
	SetLanguage(ResolveLanguage(settings.Language))
	specs, _ := st.Sandboxes()
	m := &Model{
		store:  st,
		Toasts: ui.NewToaster().Limit(3),

		Page:      ggui.State(PageDashboard),
		Settings:  ggui.State(settings),
		Engine:    ggui.State(Engine{Loading: true}),
		Update:    ggui.State(Update{Source: settings.UpdateChannel == ChannelSource}),
		Autostart: ggui.State(false), // read by Start
		BuildLog:  ggui.State(""),
		Now:       ggui.State(time.Now()),

		Specs:    ggui.State(specs).WithEqual(func(a, b []sandbox.Spec) bool { return slices.EqualFunc(a, b, sandbox.Spec.Equal) }),
		States:   ggui.State(map[string]sandbox.State{}).WithEqual(nil),
		Selected: ggui.State(""),
		Logs:     ggui.State([]sandbox.LogLine(nil)).WithEqual(nil),
		Detail:   ggui.State(0),
		Query:    ggui.State(""),

		Images:    ggui.State(Listing[rungv.Image]{}).WithEqual(nil),
		Volumes:   ggui.State(Listing[rungv.Volume]{}).WithEqual(nil),
		Disk:      ggui.State([]rungv.DiskUsage(nil)).WithEqual(slices.Equal[[]rungv.DiskUsage]),
		Snapshots: ggui.State([]sandbox.Snapshot(nil)).WithEqual(slices.Equal[[]sandbox.Snapshot]),

		DockerContainers: ggui.State(Listing[dockerapi.Container]{}).WithEqual(nil),
		DockerVersion:    ggui.State(dockerapi.Version{}),
		KubeconfigReady:  ggui.State(false),

		EditorOpen: ggui.State(false),
		Draft:      ggui.State(Draft{}),

		ConfirmOpen:    ggui.State(false),
		ConfirmTitle:   ggui.State(""),
		ConfirmText:    ggui.State(""),
		ConfirmLabel:   ggui.State(tr("Confirm")),
		PullOpen:       ggui.State(false),
		PullRef:        ggui.State(""),
		PullLog:        ggui.State(""),
		Pulling:        ggui.State(false),
		SearchResults:  ggui.State([]registry.Result(nil)),
		SearchFor:      ggui.State(""),
		SearchBusy:     ggui.State(false),
		SearchErr:      ggui.State(""),
		SearchTags:     ggui.State([]string(nil)),
		SearchTagsOf:   ggui.State(""),
		SearchTagsBusy: ggui.State(false),
		InspectOpen:    ggui.State(false),
		InspectTitle:   ggui.State(""),
		InspectBody:    ggui.State(""),
		VolumeOpen:     ggui.State(false),
		VolumeName:     ggui.State(""),
		VolumeDriver:   ggui.State("ext4lite"),
		PaletteOpen:    ggui.State(false),
		PaletteQuery:   ggui.State(""),
		QuitOpen:       ggui.State(false),
		SidebarNarrow:  ggui.State(settings.SidebarNarrow),
		ImageQuery:     ggui.State(""),
		SelectedImage:  ggui.State(""),
		SelectedVolume: ggui.State(""),
	}
	ggui.OnCleanup(m.Toasts.Close)
	m.sup = &sandbox.Supervisor{
		Client:  m.Client,
		DataDir: dataDir,
		Notify:  m.onEvent,
	}
	m.Lang = ggui.Map(m.Settings, func(s store.Settings) string { return ResolveLanguage(s.Language) })
	m.Rows = ggui.Combine(m.Specs, m.States, func(specs []sandbox.Spec, states map[string]sandbox.State) []Row {
		rows := make([]Row, len(specs))
		for i, s := range specs {
			rows[i] = Row{Spec: s, State: states[s.ID]}
		}
		return rows
	}).WithEqual(func(a, b []Row) bool { return slices.EqualFunc(a, b, Row.Equal) })
	m.Running = m.States.Map(func(states map[string]sandbox.State) int {
		n := 0
		for _, st := range states {
			if st.Status.Active() {
				n++
			}
		}
		return n
	})
	m.DraftError = m.Draft.Map(func(d Draft) string {
		spec, err := d.Spec()
		if err != nil {
			return err.Error()
		}
		if err := spec.Validate(); err != nil {
			return err.Error()
		}
		return ""
	})
	m.DraftLine = m.Draft.Map(func(d Draft) string {
		spec, err := d.Spec()
		if err != nil {
			return "# " + err.Error()
		}
		p := sandbox.Paths{}
		if spec.Checkpoint {
			p = sandbox.Paths{ControlSocket: "<control.sock>", CheckpointDir: "<snapshot-dir>"}
		}
		return wrapCommand(spec.RunArgs(p))
	})
	return m
}

// wrapCommand formats a rungv command over several lines, one flag and its
// value per line, as the recipes in the gvx docs are written.
func wrapCommand(args []string) string {
	var b strings.Builder
	b.WriteString("rungv")
	i := 0
	if len(args) > 0 {
		b.WriteString(" " + rungv.Quote(args[0]))
		i = 1
	}
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && takesValue(a) {
			b.WriteString(" \\\n  " + rungv.Quote(a) + " " + rungv.Quote(args[i+1]))
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-") {
			b.WriteString(" \\\n  " + rungv.Quote(a))
			i++
			continue
		}
		// The image and its command.
		b.WriteString(" \\\n  " + rungv.FormatArgs(args[i:]))
		break
	}
	return b.String()
}

func takesValue(flag string) bool {
	switch flag {
	case "-ro", "-isolate-lan", "--host-ca-certs", "--privileged=false":
		return false
	}
	return true
}

// Attach gives the model its host once the app exists.
func (m *Model) Attach(host ggui.Host) { m.host = host }

// Supervisor is the model's process supervisor.
func (m *Model) Supervisor() *sandbox.Supervisor { return m.sup }

// Client returns the current rungv client; safe from any goroutine.
func (m *Model) Client() *rungv.Client {
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	if m.client == nil {
		return &rungv.Client{}
	}
	c := *m.client
	return &c
}

func (m *Model) post(fn func()) {
	if m.host != nil {
		m.host.Post(fn)
	}
}

// async runs work on a goroutine and done with its error on the UI thread.
func (m *Model) async(work func(ctx context.Context) error, done func(error)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err := work(ctx)
		m.post(func() { done(err) })
	}()
}

func (m *Model) toastError(title string, err error) {
	desc := err.Error()
	if advice := rungv.Diagnose(desc, 0); advice != "" {
		desc += "\n" + advice
	}
	m.Toasts.Push(ui.Toast(title, desc).Error().Duration(8 * time.Second))
}

func (m *Model) toastOK(title, desc string) {
	m.Toasts.Push(ui.Toast(title, desc).Success())
}

// Confirm asks before running action.
func (m *Model) Confirm(title, text, label string, action func()) {
	m.ConfirmTitle.Set(title)
	m.ConfirmText.Set(text)
	m.ConfirmLabel.Set(label)
	m.confirmAction = action
	m.ConfirmOpen.Set(true)
}

// RunConfirmed runs the pending confirmation.
func (m *Model) RunConfirmed() {
	if fn := m.confirmAction; fn != nil {
		m.confirmAction = nil
		fn()
	}
}

// Go switches the page.
func (m *Model) Go(page string) {
	m.Page.Set(page)
	switch page {
	case PageImages:
		m.LoadImages()
	case PageVolumes:
		m.LoadVolumes()
	case PageSnapshots:
		m.LoadSnapshots()
	case PageDashboard, PageSettings:
		m.LoadDisk()
	case PageDocker:
		m.LoadDocker()
	case PageKubernetes:
		m.CheckKubeconfig()
	}
}

// ---- Engine -------------------------------------------------------------

// Start locates rungv, loads everything and starts the autostart
// sandboxes. Call it on the UI thread after Attach.
func (m *Model) Start() {
	m.refreshStates()
	if specs := ggui.Peek(m.Specs); len(specs) > 0 {
		m.Selected.Set(specs[0].ID)
	}
	m.DetectEngine(func(ok bool) {
		if !ok {
			return
		}
		for _, s := range ggui.Peek(m.Specs) {
			if s.AutoStart {
				m.StartSandbox(s.ID)
			}
		}
	})
	// A registration to start at login names the program; one that has
	// moved since is named again.
	m.refreshAutostart()
	if !ggui.Peek(m.Settings).NoUpdateCheck {
		m.CheckUpdate(true)
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			m.post(func() { m.Now.Set(time.Now()) })
		}
	}()
}

// DetectEngine looks for rungv and asks it about the host.
func (m *Model) DetectEngine(then func(ok bool)) {
	set := ggui.Peek(m.Settings)
	m.Engine.Update(func(e Engine) Engine { e.Loading = true; return e })
	type result struct {
		e Engine
		c *rungv.Client
	}
	var r result
	m.async(func(ctx context.Context) error {
		path, err := rungv.Locate(set.RungvPath)
		r.e = Engine{Path: path, Checked: true}
		if err != nil {
			r.e.Err = tr("rungv was not found. Install it, or set its path in Settings.")
			return nil
		}
		c := &rungv.Client{Path: path, CacheDir: rungv.ExpandHome(set.CacheDir)}
		r.c = c
		if v, err := c.Version(ctx); err == nil {
			r.e.Version = v
		} else {
			r.e.Err = tr("rungv did not run: ") + err.Error()
			return nil
		}
		info, err := c.Info(ctx)
		if err != nil {
			r.e.Err = tr("rungv info failed: ") + err.Error()
			return nil
		}
		r.e.Info = info
		return nil
	}, func(error) {
		if r.c != nil {
			m.clientMu.Lock()
			m.client = r.c
			m.clientMu.Unlock()
		}
		m.Engine.Set(r.e)
		ok := r.e.Path != "" && r.e.Err == ""
		if ok {
			m.LoadImages()
			m.LoadVolumes()
			m.LoadDisk()
			m.LoadSnapshots()
		}
		if then != nil {
			then(ok && len(r.e.Info.AvailableBackends) > 0)
		}
	})
}

// SignRungv signs the binary with the hypervisor entitlement (macOS).
func (m *Model) SignRungv() {
	path := ggui.Peek(m.Engine).Path
	if path == "" {
		return
	}
	m.async(func(ctx context.Context) error {
		return rungv.Sign(ctx, path)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Signing failed"), err)
			return
		}
		m.toastOK(tr("rungv signed"), tr("Hypervisor.framework access is granted."))
		m.DetectEngine(nil)
	})
}

// ---- Sandboxes ----------------------------------------------------------

func (m *Model) onEvent(e sandbox.Event) {
	switch e.Kind {
	case sandbox.EventReady, sandbox.EventExited:
		m.post(func() {
			m.refreshStates()
			m.announce(e)
		})
	default:
		m.dirty.Store(true)
		if m.scheduled.CompareAndSwap(false, true) {
			time.AfterFunc(120*time.Millisecond, func() {
				m.scheduled.Store(false)
				if m.dirty.Swap(false) {
					m.post(m.refreshStates)
				}
			})
		}
	}
}

func (m *Model) announce(e sandbox.Event) {
	spec, ok := m.spec(e.ID)
	if !ok || m.quitting {
		return
	}
	st := m.sup.State(e.ID)
	switch e.Kind {
	case sandbox.EventReady:
		m.toastOK(tr("%s is ready", spec.Name), readyText(spec))
		if spec.Recipe == recipes.Docker {
			m.LoadDocker()
		}
		if spec.Recipe == recipes.Kubernetes {
			m.CheckKubeconfig()
		}
	case sandbox.EventExited:
		switch st.Status {
		case sandbox.Failed:
			desc := st.Error
			if st.Advice != "" {
				desc = st.Advice + "\n" + st.Error
			}
			m.Toasts.Push(ui.Toast(fmt.Sprintf(tr("%s failed (exit %d)"), spec.Name, st.ExitCode), desc).Error().Duration(10*time.Second).
				Action(tr("Logs"), func() { m.ShowSandbox(e.ID, 0) }))
		case sandbox.Exited:
			m.Toasts.Push(ui.Toast(tr("%s exited", spec.Name), tr("The guest command finished with code 0.")).Info())
		}
		m.LoadVolumes()
		m.LoadSnapshots()
	}
}

func readyText(s sandbox.Spec) string {
	switch s.Recipe {
	case recipes.Docker:
		api, ok := recipes.DockerAPIOf(s)
		if !ok {
			api = recipes.DefaultDockerAPI()
		}
		return tr("The Docker Engine accepts connections on %s.", api.Host())
	case recipes.Kubernetes:
		return tr("kubectl can reach the cluster with KUBECONFIG=%s.", recipes.Kubeconfig())
	}
	return tr("The sandbox is running.")
}

// refreshStates copies the supervisor's states and the selected logs into
// signals.
func (m *Model) refreshStates() {
	specs := ggui.Peek(m.Specs)
	states := make(map[string]sandbox.State, len(specs))
	for _, s := range specs {
		states[s.ID] = m.sup.State(s.ID)
	}
	m.States.Set(states)
	m.refreshLogs()
}

func (m *Model) refreshLogs() {
	id := ggui.Peek(m.Selected)
	if id == "" {
		if m.logSeq != -1 {
			m.Logs.Set(nil)
			m.logSeq = -1
		}
		return
	}
	logs := m.sup.Logs(id)
	if seq := logs.Seq(); seq != m.logSeq {
		m.logSeq = seq
		m.Logs.Set(logs.Snapshot())
	}
}

// Select shows a sandbox in the detail panel.
func (m *Model) Select(id string) {
	if ggui.Peek(m.Selected) == id {
		return
	}
	m.Selected.Set(id)
	m.logSeq = -2
	m.refreshLogs()
}

// ShowSandbox opens the sandboxes page on a sandbox and detail tab.
func (m *Model) ShowSandbox(id string, tab int) {
	m.Select(id)
	m.Detail.Set(tab)
	m.Go(PageSandboxes)
}

func (m *Model) spec(id string) (sandbox.Spec, bool) {
	for _, s := range ggui.Peek(m.Specs) {
		if s.ID == id {
			return s, true
		}
	}
	return sandbox.Spec{}, false
}

func (m *Model) saveSpecs(specs []sandbox.Spec) {
	m.Specs.Set(specs)
	if err := m.store.SaveSandboxes(specs); err != nil {
		m.toastError(tr("Could not save sandboxes"), err)
	}
	m.refreshStates()
}

// StartSandbox starts a saved sandbox.
func (m *Model) StartSandbox(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	if !ggui.Peek(m.Engine).Ready() {
		m.toastError(tr("The engine is not ready"), errors.New(engineProblem(ggui.Peek(m.Engine))))
		return
	}
	if err := m.sup.Start(spec); err != nil {
		m.toastError(tr("Could not start %s", spec.Name), err)
		return
	}
	m.refreshStates()
	m.LoadVolumes()
}

func engineProblem(e Engine) string {
	switch {
	case e.Err != "":
		return e.Err
	case len(e.Info.AvailableBackends) == 0:
		return tr("rungv found no execution backend on this host.")
	}
	return tr("rungv is still being checked.")
}

// StopSandbox stops a running sandbox.
func (m *Model) StopSandbox(id string) {
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	go func() { _ = m.sup.Stop(id, timeout) }()
}

// RestartSandbox stops and starts a sandbox.
func (m *Model) RestartSandbox(id string) {
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	m.async(func(context.Context) error {
		return m.sup.Stop(id, timeout)
	}, func(error) { m.StartSandbox(id) })
}

// AskDelete confirms and removes a sandbox.
func (m *Model) AskDelete(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	text := tr("The sandbox definition and its logs are removed. Volumes and images stay.")
	if m.sup.State(id).Status.Active() {
		text = tr("The sandbox is running and will be stopped. ") + text
	}
	m.Confirm(tr("Delete %s?", spec.Name), text, tr("Delete sandbox"), func() {
		timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
		m.async(func(context.Context) error { return m.sup.Stop(id, timeout) }, func(error) {
			m.sup.Forget(id)
			specs := slices.DeleteFunc(slices.Clone(ggui.Peek(m.Specs)), func(s sandbox.Spec) bool { return s.ID == id })
			if ggui.Peek(m.Selected) == id {
				next := ""
				if len(specs) > 0 {
					next = specs[0].ID
				}
				m.Select(next)
			}
			m.saveSpecs(specs)
			m.Toasts.Push(ui.Toast(tr("Deleted %s", spec.Name), tr("Its volumes are still listed under Volumes.")).Info())
		})
	})
}

// Duplicate copies a sandbox into the editor as a new one.
func (m *Model) Duplicate(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	c := spec.Clone()
	c.ID = ""
	c.Name = uniqueName(ggui.Peek(m.Specs), spec.Name+"-copy")
	c.AutoStart = false
	m.OpenEditor(c)
}

func uniqueName(specs []sandbox.Spec, name string) string {
	taken := map[string]bool{}
	for _, s := range specs {
		taken[s.Name] = true
	}
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", name, i); !taken[n] {
			return n
		}
	}
}

// NewSandbox opens the editor on an empty sandbox, or one for image.
func (m *Model) NewSandbox(image string) {
	spec := sandbox.Spec{Image: image}
	if image != "" {
		spec.Name = uniqueName(ggui.Peek(m.Specs), sandbox.SuggestName(image))
	}
	m.OpenEditor(spec)
}

// EditSandbox opens the editor on a saved sandbox.
func (m *Model) EditSandbox(id string) {
	if spec, ok := m.spec(id); ok {
		m.OpenEditor(spec)
	}
}

// OpenEditor shows the editor sheet with spec.
func (m *Model) OpenEditor(spec sandbox.Spec) {
	m.Draft.Set(DraftFrom(spec))
	m.EditorOpen.Set(true)
}

// SaveDraft stores the editor's sandbox, and starts it with start.
func (m *Model) SaveDraft(start bool) bool {
	d := ggui.Peek(m.Draft)
	spec, err := d.Spec()
	if err == nil {
		err = spec.Validate()
	}
	if err != nil {
		m.toastError(tr("Check the sandbox settings"), err)
		return false
	}
	specs := slices.Clone(ggui.Peek(m.Specs))
	for _, s := range specs {
		if s.Name == spec.Name && s.ID != spec.ID {
			m.toastError(tr("Name in use"), errors.New(tr("another sandbox is called %s", spec.Name)))
			return false
		}
	}
	if spec.ID == "" {
		spec.ID = sandbox.NewID()
		spec.CreatedAt = time.Now()
		specs = append(specs, spec)
	} else {
		i := slices.IndexFunc(specs, func(s sandbox.Spec) bool { return s.ID == spec.ID })
		if i < 0 {
			specs = append(specs, spec)
		} else {
			if m.sup.State(spec.ID).Status.Active() && !specs[i].Equal(spec) {
				m.Toasts.Push(ui.Toast(tr("Saved %s", spec.Name), tr("Restart the sandbox to apply the changes: ports and mounts are fixed while it runs.")).Warning())
			}
			specs[i] = spec
		}
	}
	m.saveSpecs(specs)
	m.EditorOpen.Set(false)
	m.Select(spec.ID)
	if start && !m.sup.State(spec.ID).Status.Active() {
		m.StartSandbox(spec.ID)
		m.Detail.Set(0)
		m.Go(PageSandboxes)
	}
	return true
}

// CreateFromRecipe saves the preset's sandbox once and starts it.
func (m *Model) CreateFromRecipe(key string) {
	for _, s := range ggui.Peek(m.Specs) {
		if s.Recipe == key {
			m.Select(s.ID)
			if !m.sup.State(s.ID).Status.Active() {
				m.StartSandbox(s.ID)
			}
			return
		}
	}
	var spec sandbox.Spec
	switch key {
	case recipes.Docker:
		spec = recipes.DockerEngineWith(m.dockerSetting())
	case recipes.Kubernetes:
		spec = recipes.KubernetesNode()
	default:
		return
	}
	spec.Name = uniqueName(ggui.Peek(m.Specs), spec.Name)
	m.saveSpecs(append(slices.Clone(ggui.Peek(m.Specs)), spec))
	m.Select(spec.ID)
	m.StartSandbox(spec.ID)
}

// RecipeSandbox returns the sandbox made from a preset.
func (m *Model) RecipeSandbox(rows []Row, key string) (Row, bool) {
	for _, r := range rows {
		if r.Spec.Recipe == key {
			return r, true
		}
	}
	return Row{}, false
}

// ClearLogs empties the logs of a sandbox.
func (m *Model) ClearLogs(id string) {
	m.sup.Logs(id).Clear()
	m.logSeq = -2
	m.refreshLogs()
}

// ---- Snapshots ----------------------------------------------------------

// Checkpoint snapshots a running sandbox.
func (m *Model) Checkpoint(id string, leaveRunning bool) {
	spec, _ := m.spec(id)
	toast := m.Toasts.Push(ui.Toast(tr("Writing snapshot"), tr("Saving memory, tasks and volumes of %s…", spec.Name)).Loading())
	var dir string
	m.async(func(ctx context.Context) error {
		var err error
		dir, err = m.sup.Checkpoint(ctx, id, leaveRunning)
		return err
	}, func(err error) {
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Snapshot failed"), err.Error()).Error())
			return
		}
		what := tr("The sandbox stopped after the snapshot.")
		if leaveRunning {
			what = tr("The sandbox keeps running.")
		}
		m.Toasts.Update(toast, ui.Toast(tr("Snapshot saved"), what+tr(" Restore it from the Snapshots page.")).Success())
		_ = dir
		m.LoadSnapshots()
		m.refreshStates()
	})
}

// LoadSnapshots lists the bundles of every sandbox.
func (m *Model) LoadSnapshots() {
	specs := ggui.Peek(m.Specs)
	var snaps []sandbox.Snapshot
	m.async(func(context.Context) error {
		for _, s := range specs {
			snaps = append(snaps, m.sup.Snapshots(s.ID)...)
		}
		slices.SortFunc(snaps, func(a, b sandbox.Snapshot) int { return b.Time.Compare(a.Time) })
		return nil
	}, func(error) { m.Snapshots.Set(snaps) })
}

// Restore resumes a snapshot.
func (m *Model) Restore(snap sandbox.Snapshot) {
	spec, ok := m.spec(snap.SandboxID)
	if !ok {
		m.toastError(tr("Cannot restore"), errors.New(tr("the sandbox of this snapshot was deleted")))
		return
	}
	if m.sup.State(spec.ID).Status.Active() {
		m.toastError(tr("Cannot restore"), errors.New(tr("stop %s first", spec.Name)))
		return
	}
	if err := m.sup.Restore(spec, snap.Dir); err != nil {
		m.toastError(tr("Could not restore %s", spec.Name), err)
		return
	}
	m.ShowSandbox(spec.ID, 0)
	m.refreshStates()
}

// DeleteSnapshot removes a bundle after confirmation.
func (m *Model) DeleteSnapshot(snap sandbox.Snapshot) {
	m.Confirm(tr("Delete snapshot?"), tr("The bundle %s (%s) is removed from disk.", snap.Name, humanBytes(snap.Size)), tr("Delete snapshot"), func() {
		m.async(func(context.Context) error { return removeAll(snap.Dir) }, func(err error) {
			if err != nil {
				m.toastError(tr("Could not delete the snapshot"), err)
			}
			m.LoadSnapshots()
		})
	})
}
