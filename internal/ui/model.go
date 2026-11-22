// Package ui is the desktop app's interface, built with ggui.
package ui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/dockerproxy"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
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
	PageContainers = "containers"
	PageNetworks   = "networks"
	PagePods       = "pods"
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
	store   store.Store
	sup     *sandbox.Supervisor
	host    ggui.Host
	appMenu func() []ggui.MenuItem

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
	// AppUpdate is what the app knows of its own newest release; see
	// model_appupdate.go.
	AppUpdate  *ggui.StateValue[AppUpdate]
	appRelease appupdate.Release // what AppUpdate tells of
	appStaged  *appupdate.Staged // downloaded, for a restart to put in place
	appRestart *appupdate.Staged // to finish once the app has ended
	appChecked time.Time         // when the app's releases were last asked for, by the wall clock
	Now        *ggui.StateValue[time.Time]
	// Lang is the language the app is worded in, from the settings or the
	// system: one of LanguageTags.
	Lang *ggui.DerivedValue[string]

	Specs             *ggui.StateValue[[]sandbox.Spec]
	States            *ggui.StateValue[map[string]sandbox.State]
	Rows              *ggui.DerivedValue[[]Row]
	Running           *ggui.DerivedValue[int]
	EngineRunning     *ggui.StateValue[int]
	TotalRunning      *ggui.DerivedValue[int]
	EngineVolumeUsers *ggui.StateValue[map[string][]string]
	BuilderState      *ggui.StateValue[engine.BuilderState]
	BuildxStatus      *ggui.StateValue[BuildxStatus]
	Amd64State        *ggui.StateValue[engine.Amd64State]
	Amd64Check        *ggui.StateValue[Amd64CheckState]
	Amd64Resetting    *ggui.StateValue[bool]
	Selected          *ggui.StateValue[string]
	Logs              *ggui.StateValue[[]sandbox.LogLine]
	Detail            *ggui.StateValue[int]
	Query             *ggui.StateValue[string]

	Images    *ggui.StateValue[Listing[rungv.Image]]
	Volumes   *ggui.StateValue[Listing[rungv.Volume]]
	Disk      *ggui.StateValue[[]rungv.DiskUsage]
	Snapshots *ggui.StateValue[[]sandbox.Snapshot]

	// ImageView chooses between gvx's own image cache (ImagesGVX) and the
	// images inside the Docker Engine sandbox (ImagesDocker).
	ImageView           *ggui.StateValue[int]
	DockerImages        *ggui.StateValue[Listing[dockerapi.Image]]
	DockerImageQuery    *ggui.StateValue[string]
	SelectedDockerImage *ggui.StateValue[string]

	DockerContainers *ggui.StateValue[Listing[dockerapi.Container]]
	DockerVersion    *ggui.StateValue[dockerapi.Version]
	// ContainersAccess says whether the Containers page can be used, which
	// is while the Docker Engine runs, and ContainersOpen is its yes.
	ContainersAccess *ggui.DerivedValue[ContainersAccess]
	ContainersOpen   *ggui.DerivedValue[bool]
	// RunningContainers counts the engine's running containers.
	RunningContainers *ggui.DerivedValue[int]
	// The Containers page: what filters its table, the container chosen in
	// it, which is the first when none is, the tab of its details, and the
	// last lines of its logs.
	ContainerQuery    *ggui.StateValue[string]
	SelectedContainer *ggui.StateValue[string]
	Container         *ggui.DerivedValue[dockerapi.Container]
	ContainerDetail   *ggui.StateValue[int]
	ContainerLogs     *ggui.StateValue[ContainerLogs]
	// ComposeFolded are the Compose projects whose containers the table
	// folds under their row.
	ComposeFolded *ggui.StateValue[map[string]bool]
	// The Networks page: the engine's networks, what filters them, and the
	// one chosen (by ID or name), which is the first when none is.
	DockerNetworks  *ggui.StateValue[Listing[dockerapi.Network]]
	NetworkQuery    *ggui.StateValue[string]
	SelectedNetwork *ggui.StateValue[string]
	Network         *ggui.DerivedValue[dockerapi.Network]
	KubeconfigReady *ggui.StateValue[bool]
	// Pods are the k3s cluster's, and PodsWaiting says its sandbox runs
	// and its API server has yet to answer: k3s is still coming up.
	Pods        *ggui.StateValue[Listing[kubeapi.Pod]]
	PodsWaiting *ggui.StateValue[bool]
	// PodsAccess says whether the Pods page can be used, which is while
	// the cluster's sandbox runs, and PodsOpen is its yes.
	PodsAccess *ggui.DerivedValue[PodsAccess]
	PodsOpen   *ggui.DerivedValue[bool]
	// RunningPods counts the cluster's running pods.
	RunningPods *ggui.DerivedValue[int]
	// The Pods page: what filters its table, whether it shows the system's
	// pods, the pod chosen in it (namespace/name), which is the first shown
	// when none is, the tab of its details, the container whose logs show,
	// as chosen and as it is, and the last lines of those.
	PodQuery        *ggui.StateValue[string]
	SystemPods      *ggui.StateValue[bool]
	SelectedPod     *ggui.StateValue[string]
	Pod             *ggui.DerivedValue[kubeapi.Pod]
	PodDetail       *ggui.StateValue[int]
	PodContainer    *ggui.StateValue[string]
	PodLogContainer *ggui.DerivedValue[string]
	PodLogs         *ggui.StateValue[ContainerLogs]
	// KubeView is the view of the cluster the Pods page shows (KubePods,
	// KubeWorkloads or KubeServices), and PodOwner the workload whose pods
	// alone the Pods view shows, if one is set.
	KubeView *ggui.StateValue[int]
	PodOwner *ggui.StateValue[WorkloadRef]
	// Workloads and Services are the cluster's, read with its pods while a
	// page shows them; the queries filter their tables.
	Workloads     *ggui.StateValue[Listing[kubeapi.Workload]]
	Services      *ggui.StateValue[Listing[kubeapi.Service]]
	WorkloadQuery *ggui.StateValue[string]
	ServiceQuery  *ggui.StateValue[string]
	// What the running sandboxes (by ID), containers (by ID) and pods (by
	// namespace/name) use, read while a page shows them; see model_usage.go.
	SandboxUsage   *ggui.StateValue[map[string]Usage]
	ContainerUsage *ggui.StateValue[map[string]Usage]
	PodUsage       *ggui.StateValue[map[string]PodUse]
	// Forwards are the port forwards that run, oldest first.
	Forwards *ggui.StateValue[[]PortForward]
	// The dialog that starts a port forward: what to, the ports typed, and
	// why it could not start.
	ForwardOpen   *ggui.StateValue[bool]
	ForwardDraft  *ggui.StateValue[ForwardDraft]
	ForwardRemote *ggui.StateValue[string]
	ForwardLocal  *ggui.StateValue[string]
	ForwardErr    *ggui.StateValue[string]
	// The dialog that scales a workload.
	ScaleOpen     *ggui.StateValue[bool]
	ScaleTarget   *ggui.StateValue[kubeapi.Workload]
	ScaleReplicas *ggui.StateValue[float64]
	// DockerEndpoint is the DOCKER_HOST value that reaches the Docker
	// Engine, as DockerHost says it.
	DockerEndpoint *ggui.DerivedValue[string]
	// DockerProxyErr is the last listener error from the app's Docker API
	// proxy, or empty when listening normally.
	DockerProxyErr *ggui.StateValue[string]
	// DockerLink and KubeLink are the contexts docker and kubectl on this
	// machine reach the recipes' engines through.
	DockerLink, KubeLink *Link
	// LinkCommandsOpen unfolds the commands that set the contexts up by hand.
	LinkCommandsOpen *ggui.StateValue[bool]
	// Menus is what the Docker and Kubernetes menus tell; see RecipeMenu.
	Menus *ggui.DerivedValue[MenuState]

	// Editor
	EditorOpen   *ggui.StateValue[bool]
	Draft        *ggui.StateValue[Draft]
	DraftError   *ggui.DerivedValue[string]
	DraftLine    *ggui.DerivedValue[string]
	DraftRunning *ggui.DerivedValue[bool]

	// Dialogs
	ConfirmOpen   *ggui.StateValue[bool]
	ConfirmTitle  *ggui.StateValue[string]
	ConfirmText   *ggui.StateValue[string]
	ConfirmLabel  *ggui.StateValue[string]
	ConfirmSafe   *ggui.StateValue[bool] // the action destroys nothing: its button is not red
	confirmAction func()
	PullOpen      *ggui.StateValue[bool]
	PullTarget    *ggui.StateValue[int]
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
	Offline bool
	// Image explorer dialog state (Files, Layers, Inspect).
	ExploreOpen     *ggui.StateValue[bool]
	ExploreTitle    *ggui.StateValue[string]
	ExploreSource   *ggui.StateValue[string]
	ExploreTab      *ggui.StateValue[int]
	ExploreLoading  *ggui.StateValue[bool]
	ExploreProgress *ggui.StateValue[string]
	ExploreErr      *ggui.StateValue[string]
	ExploreLayers   *ggui.StateValue[[]imagefs.Layer]
	ExploreLayer    *ggui.StateValue[int]
	ExplorePath     *ggui.StateValue[string]
	ExploreQuery    *ggui.StateValue[string]
	ExploreEntries  *ggui.StateValue[[]imagefs.Entry]
	ExploreSelected *ggui.StateValue[string]
	ExplorePreview  *ggui.StateValue[FilePreview]
	ExploreInspect  *ggui.StateValue[string]
	explorer        explorerSession
	VolumeOpen      *ggui.StateValue[bool]
	VolumeName      *ggui.StateValue[string]
	VolumeDriver    *ggui.StateValue[string]
	PaletteOpen     *ggui.StateValue[bool]
	PaletteQuery    *ggui.StateValue[string]
	QuitOpen        *ggui.StateValue[bool]
	SidebarNarrow   *ggui.StateValue[bool]
	SandboxStatus   *ggui.StateValue[string]
	ImageQuery      *ggui.StateValue[string]
	VolumeQuery     *ggui.StateValue[string]
	SnapshotQuery   *ggui.StateValue[string]
	ExploreSplit    *ggui.StateValue[float64]
	SelectedImage   *ggui.StateValue[string]
	SelectedVolume  *ggui.StateValue[string]

	dirty     atomic.Bool
	scheduled atomic.Bool
	logSeq    int64
	quitting  bool

	runCLI                                     cliRunner                  // runs docker and kubectl; tests replace it
	runVMHelper                                vmHelperRunner             // runs __vm-helper check/install-rosetta; tests replace it
	syncAmd64NodeHook                          func(on bool)              // overrides syncAmd64Node in tests
	dockerLoading, dockerAgain                 bool                       // LoadDocker runs; it is wanted again after
	dockerNetworksLoading, dockerNetworksAgain bool                       // LoadDockerNetworks runs; it is wanted again after
	dockerImagesLoading, dockerImagesAgain     bool                       // LoadDockerImages runs; it is wanted again after
	logsLoading, logsAgain                     bool                       // LoadContainerLogs runs; it is wanted again after
	podsLoading, podsAgain                     bool                       // LoadPods runs; it is wanted again after
	podLogsLoading, podLogsAgain               bool                       // LoadPodLogs runs; it is wanted again after
	podsAnswered                               bool                       // the cluster's API server answered since it started
	sandboxUsageLoading                        bool                       // LoadSandboxUsage runs
	containerUsageLoading                      bool                       // LoadContainerUsage runs
	sandboxSamples                             map[string]sandboxSample   // the last of each sandbox's, by ID
	containerStats                             map[string]dockerapi.Stats // the last of each container's, by ID
	terms                                      liveTerminals
	forwards                                   liveForwards
	forwarder                                  forwarder // carries port forwards; tests replace it
	dockerProxy                                *dockerproxy.Proxy
	dockerProxyStarted                         bool
	dockerBackend                              string // overrides recipes.DockerBackendHost() in tests
	engineSpec                                 atomic.Pointer[sandbox.Spec]
	sandboxEngine                              sandboxEngine // in-app Docker Engine API (*engine.Engine); tests may replace
	sandboxEngineGen                           int
	amd64Gen                                   int
	buildxSyncGen                              int
	keepImages                                 atomic.Pointer[[]string]
}

// NewModel loads the saved state. Call it under a ggui owner (ggui.Root).
func NewModel(st store.Store, dataDir string) *Model {
	settings, _ := st.Settings()
	SetLanguage(ResolveLanguage(settings.Language))
	specs, _ := st.Sandboxes()
	if settings.DockerEndpoint == "" && settings.DockerPort == 0 {
		for _, s := range specs {
			if s.Recipe != recipes.Docker {
				continue
			}
			if api, ok := recipes.LegacyDockerAPI(s); ok && api.TCP {
				settings.DockerEndpoint = DockerTCPEndpoint
				settings.DockerPort = api.Port
				_ = st.SaveSettings(settings)
			}
			break
		}
	}
	if up, changed := upgradeRecipes(specs); changed {
		if err := st.SaveSandboxes(up); err == nil {
			specs = up
		}
	}
	m := &Model{
		store:  st,
		Toasts: ui.NewToaster().Limit(3),

		Page:      ggui.State(PageDashboard),
		Settings:  ggui.State(settings),
		Engine:    ggui.State(Engine{Loading: true}),
		Update:    ggui.State(Update{Source: settings.UpdateChannel == ChannelSource, Repo: rungv.Source{Repo: settings.SourceRepo, Branch: settings.SourceBranch}.DisplayRepo(), Branch: rungv.Source{Repo: settings.SourceRepo, Branch: settings.SourceBranch}.BranchName()}),
		Autostart: ggui.State(false), // read by Start
		BuildLog:  ggui.State(""),
		AppUpdate: ggui.State(AppUpdate{Install: appupdate.Detect()}),
		Now:       ggui.State(time.Now()),

		Specs:             ggui.State(specs).WithEqual(func(a, b []sandbox.Spec) bool { return slices.EqualFunc(a, b, sandbox.Spec.Equal) }),
		States:            ggui.State(map[string]sandbox.State{}).WithEqual(nil),
		EngineRunning:     ggui.State(0),
		EngineVolumeUsers: ggui.State(map[string][]string(nil)).WithEqual(stringSliceMapEqual),
		BuilderState:      ggui.State(engine.BuilderState{Status: engine.BuilderIdle}),
		BuildxStatus:      ggui.State(BuildxStatus{}).WithEqual(BuildxStatus.Equal),
		Amd64State:        ggui.State(engine.Amd64State{Status: engine.BuilderIdle}).WithEqual(engine.Amd64State.Equal),
		Amd64Check:        ggui.State(Amd64CheckState{}).WithEqual(Amd64CheckState.Equal),
		Amd64Resetting:    ggui.State(false),
		Selected:          ggui.State(""),
		Logs:              ggui.State([]sandbox.LogLine(nil)).WithEqual(nil),
		Detail:            ggui.State(0),
		Query:             ggui.State(""),

		Images:              ggui.State(Listing[rungv.Image]{}).WithEqual(nil),
		Volumes:             ggui.State(Listing[rungv.Volume]{}).WithEqual(nil),
		Disk:                ggui.State([]rungv.DiskUsage(nil)).WithEqual(slices.Equal[[]rungv.DiskUsage]),
		Snapshots:           ggui.State([]sandbox.Snapshot(nil)).WithEqual(slices.Equal[[]sandbox.Snapshot]),
		ImageView:           ggui.State(ImagesGVX),
		DockerImages:        ggui.State(Listing[dockerapi.Image]{}).WithEqual(nil),
		DockerImageQuery:    ggui.State(""),
		SelectedDockerImage: ggui.State(""),

		DockerContainers:  ggui.State(Listing[dockerapi.Container]{}).WithEqual(nil),
		DockerVersion:     ggui.State(dockerapi.Version{}),
		DockerProxyErr:    ggui.State(""),
		ContainerQuery:    ggui.State(""),
		SelectedContainer: ggui.State(""),
		ContainerDetail:   ggui.State(0),
		ContainerLogs:     ggui.State(ContainerLogs{}).WithEqual(ContainerLogs.Equal),
		ComposeFolded:     ggui.State(map[string]bool{}).WithEqual(maps.Equal[map[string]bool]),
		DockerNetworks:    ggui.State(Listing[dockerapi.Network]{}).WithEqual(nil),
		NetworkQuery:      ggui.State(""),
		SelectedNetwork:   ggui.State(""),
		SandboxUsage:      ggui.State(map[string]Usage(nil)).WithEqual(usageEqual),
		ContainerUsage:    ggui.State(map[string]Usage(nil)).WithEqual(usageEqual),
		PodUsage:          ggui.State(map[string]PodUse(nil)).WithEqual(podUsageEqual),
		KubeconfigReady:   ggui.State(false),
		Pods:              ggui.State(Listing[kubeapi.Pod]{}).WithEqual(podsEqual),
		PodsWaiting:       ggui.State(false),
		PodQuery:          ggui.State(""),
		SystemPods:        ggui.State(false),
		SelectedPod:       ggui.State(""),
		PodDetail:         ggui.State(0),
		PodContainer:      ggui.State(""),
		PodLogs:           ggui.State(ContainerLogs{}).WithEqual(ContainerLogs.Equal),
		KubeView:          ggui.State(KubePods),
		PodOwner:          ggui.State(WorkloadRef{}),
		Workloads:         ggui.State(Listing[kubeapi.Workload]{}).WithEqual(workloadsEqual),
		Services:          ggui.State(Listing[kubeapi.Service]{}).WithEqual(servicesEqual),
		WorkloadQuery:     ggui.State(""),
		ServiceQuery:      ggui.State(""),
		Forwards:          ggui.State([]PortForward(nil)).WithEqual(slices.Equal[[]PortForward]),
		ForwardOpen:       ggui.State(false),
		ForwardDraft:      ggui.State(ForwardDraft{}).WithEqual(ForwardDraft.Equal),
		ForwardRemote:     ggui.State(""),
		ForwardLocal:      ggui.State(""),
		ForwardErr:        ggui.State(""),
		ScaleOpen:         ggui.State(false),
		ScaleTarget:       ggui.State(kubeapi.Workload{}).WithEqual(kubeapi.Workload.Equal),
		ScaleReplicas:     ggui.State(1.0),
		LinkCommandsOpen:  ggui.State(false),

		EditorOpen: ggui.State(false),
		Draft:      ggui.State(Draft{}),

		ConfirmOpen:     ggui.State(false),
		ConfirmTitle:    ggui.State(""),
		ConfirmText:     ggui.State(""),
		ConfirmLabel:    ggui.State(tr("Confirm")),
		ConfirmSafe:     ggui.State(false),
		PullOpen:        ggui.State(false),
		PullTarget:      ggui.State(PullTargetGVX),
		PullRef:         ggui.State(""),
		PullLog:         ggui.State(""),
		Pulling:         ggui.State(false),
		SearchResults:   ggui.State([]registry.Result(nil)),
		SearchFor:       ggui.State(""),
		SearchBusy:      ggui.State(false),
		SearchErr:       ggui.State(""),
		SearchTags:      ggui.State([]string(nil)),
		SearchTagsOf:    ggui.State(""),
		SearchTagsBusy:  ggui.State(false),
		ExploreOpen:     ggui.State(false),
		ExploreTitle:    ggui.State(""),
		ExploreSource:   ggui.State(""),
		ExploreTab:      ggui.State(ExploreFiles),
		ExploreLoading:  ggui.State(false),
		ExploreProgress: ggui.State(""),
		ExploreErr:      ggui.State(""),
		ExploreLayers:   ggui.State([]imagefs.Layer(nil)).WithEqual(func(a, b []imagefs.Layer) bool { return slices.EqualFunc(a, b, imagefs.Layer.Equal) }),
		ExploreLayer:    ggui.State(0),
		ExplorePath:     ggui.State("/"),
		ExploreQuery:    ggui.State(""),
		ExploreEntries:  ggui.State([]imagefs.Entry(nil)).WithEqual(func(a, b []imagefs.Entry) bool { return slices.EqualFunc(a, b, imagefs.Entry.Equal) }),
		ExploreSelected: ggui.State(""),
		ExplorePreview:  ggui.State(FilePreview{}).WithEqual(FilePreview.Equal),
		ExploreInspect:  ggui.State(""),
		VolumeOpen:      ggui.State(false),
		VolumeName:      ggui.State(""),
		VolumeDriver:    ggui.State("ext4lite"),
		PaletteOpen:     ggui.State(false),
		PaletteQuery:    ggui.State(""),
		QuitOpen:        ggui.State(false),
		SidebarNarrow:   ggui.State(settings.SidebarNarrow),
		SandboxStatus:   ggui.State("all"),
		ImageQuery:      ggui.State(""),
		VolumeQuery:     ggui.State(""),
		SnapshotQuery:   ggui.State(""),
		ExploreSplit:    ggui.State(0.47),
		SelectedImage:   ggui.State(""),
		SelectedVolume:  ggui.State(""),
		runCLI:          runCLI,
		runVMHelper:     defaultRunVMHelper,
		forwarder:       forwardThroughCluster,
	}
	ggui.OnCleanup(m.Toasts.Close)
	m.sup = &sandbox.Supervisor{
		Client:  m.Client,
		DataDir: dataDir,
		Notify:  m.onEvent,
		Prepare: recipes.Prepare,
	}
	m.Lang = ggui.Map(m.Settings, func(s store.Settings) string { return ResolveLanguage(s.Language) })
	m.Rows = ggui.Combine(m.Specs, m.States, func(specs []sandbox.Spec, states map[string]sandbox.State) []Row {
		rows := make([]Row, len(specs))
		for i, s := range specs {
			rows[i] = Row{Spec: s, State: states[s.ID]}
		}
		return rows
	}).WithEqual(func(a, b []Row) bool { return slices.EqualFunc(a, b, Row.Equal) })
	m.initDockerProxy(dataDir)
	m.DockerEndpoint = ggui.Map(m.Settings, func(set store.Settings) string {
		return dockerSettingOf(set).Host()
	})
	m.newLinks()
	m.ContainersAccess = ggui.Combine(m.Rows, m.Settings, containersAccess)
	m.ContainersOpen = ggui.Map(m.ContainersAccess, func(a ContainersAccess) bool { return a == ContainersOpen })
	m.RunningContainers = ggui.Map(m.DockerContainers, func(l Listing[dockerapi.Container]) int {
		n := 0
		for _, c := range l.Items {
			if c.State == "running" {
				n++
			}
		}
		return n
	})
	m.Container = ggui.Combine(m.DockerContainers, m.SelectedContainer, func(l Listing[dockerapi.Container], id string) dockerapi.Container {
		return chosenContainer(l.Items, id)
	}).WithEqual(dockerapi.Container.Equal)
	m.Network = ggui.Combine(m.DockerNetworks, m.SelectedNetwork, func(l Listing[dockerapi.Network], id string) dockerapi.Network {
		return chosenNetwork(l.Items, id)
	}).WithEqual(dockerapi.Network.Equal)
	m.PodsAccess = ggui.Map(m.Rows, podsAccess)
	m.PodsOpen = ggui.Map(m.PodsAccess, func(a PodsAccess) bool { return a == PodsOpen })
	m.RunningPods = ggui.Map(m.Pods, func(l Listing[kubeapi.Pod]) int {
		running, _, _ := podCounts(l.Items)
		return running
	})
	m.Pod = ggui.Derived(func() kubeapi.Pod {
		return chosenPod(ownedPods(userPods(m.Pods.Get().Items, m.SystemPods.Get()), m.PodOwner.Get()), m.SelectedPod.Get())
	}).WithEqual(kubeapi.Pod.Equal)
	m.PodLogContainer = ggui.Combine(m.Pod, m.PodContainer, podLogContainer)
	m.Menus = ggui.Derived(func() MenuState {
		ctrs := m.DockerContainers.Get()
		s := menuState(m.Rows.Get(), ctrs, m.Pods.Get(), m.PodsWaiting.Get(), m.DockerLink.View.Get(), m.KubeLink.View.Get())
		if m.Settings.Get().SandboxEngine {
			s.SandboxEngine = true
			s.menuContainers(ctrs)
		}
		s.Forwards = menuForwards(m.Forwards.Get())
		return s
	}).WithEqual(MenuState.Equal)
	m.Running = m.States.Map(func(states map[string]sandbox.State) int {
		n := 0
		for _, st := range states {
			if st.Status.Active() {
				n++
			}
		}
		return n
	})
	m.TotalRunning = ggui.Combine(m.Running, m.EngineRunning, func(a, b int) int { return a + b })
	m.DraftError = ggui.Combine(m.Draft, m.Specs, func(d Draft, specs []sandbox.Spec) string {
		spec, err := d.Spec()
		if err != nil {
			return err.Error()
		}
		if err := spec.Validate(); err != nil {
			return err.Error()
		}
		for _, s := range specs {
			if s.Name == spec.Name && s.ID != spec.ID {
				return tr("another sandbox is called %s", spec.Name)
			}
		}
		return ""
	})
	m.DraftRunning = ggui.Combine(m.Draft, m.States, func(d Draft, states map[string]sandbox.State) bool {
		return d.ID != "" && states[d.ID].Status.Active()
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
	case "-ro", "-isolate-lan", "--host-ca-certs", "--privileged=false", "-auto-publish", "--auto-publish":
		return false
	}
	return true
}

// Attach gives the model its host once the app exists.
func (m *Model) Attach(host ggui.Host) { m.host = host }

// SetMenuSource supplies the application menu items for non-macOS windows
// that render ui.AppMenubar inside the window frame.
func (m *Model) SetMenuSource(fn func() []ggui.MenuItem) { m.appMenu = fn }

func (m *Model) appMenus() []ggui.MenuItem {
	if m.appMenu == nil {
		return nil
	}
	return m.appMenu()
}

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
	m.ConfirmSafe.Set(false)
	m.ConfirmOpen.Set(true)
}

// Offer asks before running an action that destroys nothing, such as
// starting what another action needs.
func (m *Model) Offer(title, text, label string, action func()) {
	m.Confirm(title, text, label, action)
	m.ConfirmSafe.Set(true)
}

// RunConfirmed runs the pending confirmation.
func (m *Model) RunConfirmed() {
	if fn := m.confirmAction; fn != nil {
		m.confirmAction = nil
		fn()
	}
}

// Go switches the page. The Containers page opens only while the Docker
// Engine runs, and the Pods page only while the cluster does, as the
// sidebar and the menus that lead there say.
func (m *Model) Go(page string) {
	if (page == PageContainers || page == PageNetworks) && ggui.Peek(m.ContainersAccess) != ContainersOpen ||
		page == PagePods && ggui.Peek(m.PodsAccess) != PodsOpen {
		return
	}
	m.Page.Set(page)
	switch page {
	case PageImages:
		m.LoadImages()
		m.LoadDockerImages()
	case PageVolumes:
		m.LoadVolumes()
	case PageSnapshots:
		m.LoadSnapshots()
	case PageDashboard:
		m.LoadDisk()
	case PageSettings:
		m.LoadDisk()
		m.CheckAmd64()
		m.refreshAmd64State()
	case PageDocker:
		m.LoadDocker()
		m.LoadDockerImages()
	case PageContainers:
		m.LoadDocker()
	case PageNetworks:
		m.LoadDocker()
		m.LoadDockerNetworks()
	case PageKubernetes:
		m.CheckKubeconfig()
		m.LoadPods()
	case PagePods:
		m.LoadPods()
	}
}

// connectTab is the index of the Connect tab of the sandbox details.
const connectTab = 1

// ---- Engine -------------------------------------------------------------

// Start locates rungv, loads everything and starts the autostart
// sandboxes. Call it on the UI thread after Attach.
func (m *Model) Start() {
	m.refreshStates()
	if specs := ggui.Peek(m.Specs); len(specs) > 0 {
		m.Selected.Set(specs[0].ID)
	}
	m.DetectEngine(func(ok bool) {
		if ok {
			m.startWithApp()
		}
	})
	m.startDockerProxy()
	m.followDocker()
	m.followPods()
	m.followForwards()
	m.followSandboxUsage()
	m.followContainerUsage()
	m.followPodUsage()
	m.followLinks()
	// A registration to start at login names the program; one that has
	// moved since is named again.
	m.refreshAutostart()
	if !ggui.Peek(m.Settings).NoUpdateCheck {
		m.CheckUpdate(true)
	}
	m.startAppUpdates()
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
		ready := ok && len(r.e.Info.AvailableBackends) > 0
		if ok {
			m.LoadImages()
			m.LoadVolumes()
			m.LoadDisk()
			m.LoadSnapshots()
		}
		if ready && ggui.Peek(m.Settings).SandboxEngine {
			m.openSandboxEngine()
		}
		if then != nil {
			then(ready)
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
	case sandbox.EventPortBusy:
		m.post(func() { m.announce(e) })
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
	case sandbox.EventPortBusy:
		_, port, _ := net.SplitHostPort(e.Port)
		n, err := strconv.Atoi(port)
		if err != nil {
			return
		}
		m.Toasts.Push(ui.Toast(tr("Port %d is already in use on this computer", n),
			tr("A container publishes it, and will not be reachable at 127.0.0.1:%d until what uses it stops.", n)).Warning().Duration(8*time.Second).
			Action(tr("Logs"), func() { m.ShowSandbox(e.ID, 0) }))
	case sandbox.EventReady:
		m.toastOK(tr("%s is ready", spec.Name), m.readyText(spec))
		if spec.Recipe == recipes.Kubernetes {
			m.CheckKubeconfig()
			m.CheckLink(m.KubeLink) // it has written its kubeconfig now
		}
	case sandbox.EventExited:
		// An engine that ended before it was ever ready is not started again
		// with the app, to fail there again; one that ended after it was up
		// is, as the user left it running: it may have been ended with the
		// session at a logout.
		if (e.State.Status == sandbox.Failed || e.State.Status == sandbox.Exited) && e.State.ReadyAt.IsZero() {
			m.setResume(e.ID, false)
		}
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

func (m *Model) readyText(s sandbox.Spec) string {
	switch s.Recipe {
	case recipes.Docker:
		return tr("The Docker Engine accepts connections on %s.", m.DockerHost())
	case recipes.Kubernetes:
		return tr("kubectl can reach the cluster with KUBECONFIG=%s.", recipes.Kubeconfig())
	case recipes.Registry:
		return tr("Push images to %s from the Docker Engine; pods pull them from there.", recipes.RegistryHost())
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
	m.setResume(id, true)
	m.refreshStates()
	m.LoadVolumes()
}

// startWithApp starts what starts with the app: the sandboxes set to, and
// the recipe sandboxes the user left running when the app last ended.
func (m *Model) startWithApp() {
	for _, id := range launchStarts(ggui.Peek(m.Specs), ggui.Peek(m.Settings).Resume) {
		m.StartSandbox(id)
	}
}

// launchStarts picks from specs the sandboxes to start with the app.
func launchStarts(specs []sandbox.Spec, resume []string) []string {
	var ids []string
	for _, s := range specs {
		if s.AutoStart || s.Recipe != "" && slices.Contains(resume, s.ID) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// setResume notes whether the user wants a recipe sandbox running, so that
// one left running starts again with the app. The recipe sandboxes are the
// engines the app hosts for docker and kubectl; any sandbox can be set to
// start with the app instead. It is the user's will that counts: starting
// and stopping one note it, and quitting, which stops every sandbox, does
// not.
func (m *Model) setResume(id string, on bool) {
	if spec, ok := m.spec(id); on && (!ok || spec.Recipe == "") {
		return
	}
	if slices.Contains(ggui.Peek(m.Settings).Resume, id) == on {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.Resume = slices.DeleteFunc(slices.Clone(s.Resume), func(r string) bool { return r == id })
		if on {
			s.Resume = append(s.Resume, id)
		}
	})
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

// StopSandbox stops a running sandbox, as the user asked: a recipe
// sandbox is not started again with the app.
func (m *Model) StopSandbox(id string) {
	m.setResume(id, false)
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
			m.setResume(id, false)
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
	if err == nil && spec.Recipe == recipes.Docker {
		spec = recipes.EnsureDockerBackendPort(spec)
	}
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
	active := spec.ID != "" && m.sup.State(spec.ID).Status.Active()
	if spec.ID == "" {
		spec.ID = sandbox.NewID()
		spec.CreatedAt = time.Now()
		specs = append(specs, spec)
	} else {
		i := slices.IndexFunc(specs, func(s sandbox.Spec) bool { return s.ID == spec.ID })
		if i < 0 {
			specs = append(specs, spec)
		} else {
			if active && !start && !specs[i].Equal(spec) {
				m.Toasts.Push(ui.Toast(tr("Saved %s", spec.Name), tr("Restart the sandbox to apply the changes: ports and mounts are fixed while it runs.")).Warning())
			}
			specs[i] = spec
		}
	}
	m.saveSpecs(specs)
	m.EditorOpen.Set(false)
	m.Select(spec.ID)
	if start {
		if active {
			m.RestartSandbox(spec.ID)
		} else {
			m.StartSandbox(spec.ID)
		}
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
		spec = recipes.WithSharedFolders(recipes.DockerEngine(), sharedSetting(ggui.Peek(m.Settings)))
	case recipes.Kubernetes:
		spec = recipes.KubernetesNodeWith(ggui.Peek(m.Settings).K3sServiceLB)
	case recipes.Registry:
		spec = recipes.ImageRegistry()
	default:
		return
	}
	spec.Name = uniqueName(ggui.Peek(m.Specs), spec.Name)
	m.saveSpecs(append(slices.Clone(ggui.Peek(m.Specs)), spec))
	m.Select(spec.ID)
	m.StartSandbox(spec.ID)
}

// upgradeRecipes brings the recipe sandboxes an earlier version of the app
// saved to what the recipes make now, as recipes.Upgrade does: they start
// so the next time, with what the user changed of them.
func upgradeRecipes(specs []sandbox.Spec) ([]sandbox.Spec, bool) {
	out := slices.Clone(specs)
	upgraded := false
	for i, s := range out {
		if up, changed := recipes.Upgrade(s); changed {
			out[i], upgraded = up, true
		}
	}
	return out, upgraded
}

// OpenRecipe shows what a preset's sandbox runs: the Docker and Kubernetes
// pages, and for the image registry, which has no page, its sandbox's
// Connect tab.
func (m *Model) OpenRecipe(key string) {
	switch key {
	case recipes.Docker:
		m.Go(PageDocker)
	case recipes.Kubernetes:
		m.Go(PageKubernetes)
	default:
		if r, ok := m.RecipeSandbox(ggui.Peek(m.Rows), key); ok {
			m.ShowSandbox(r.Spec.ID, connectTab)
		}
	}
}

// navRegistry is the sidebar entry of the image registry, which is not a
// page.
const navRegistry = "registry"

// OpenRegistry shows the image registry's sandbox, and offers to set it up
// when there is none.
func (m *Model) OpenRegistry() {
	if _, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Registry); ok {
		m.OpenRecipe(recipes.Registry)
		return
	}
	m.Offer(tr("Set up the image registry?"),
		tr("A registry on this computer: push images from the Docker Engine, and run them in the cluster."),
		tr("Set up and start"), func() {
			m.CreateFromRecipe(recipes.Registry)
			m.OpenRecipe(recipes.Registry)
		})
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
		if !leaveRunning {
			m.setResume(id, false) // the user stopped it with the snapshot
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
	m.setResume(spec.ID, true)
	m.ShowSandbox(spec.ID, 0)
	m.refreshStates()
}

// DeleteSnapshot removes a bundle after confirmation.
func (m *Model) DeleteSnapshot(snap sandbox.Snapshot) {
	m.Confirm(tr("Delete snapshot?"), tr("The bundle %s (%s) is removed from disk.", snap.Name, formatBytes(snap.Size)), tr("Delete snapshot"), func() {
		m.async(func(context.Context) error { return removeAll(snap.Dir) }, func(err error) {
			if err != nil {
				m.toastError(tr("Could not delete the snapshot"), err)
			}
			m.LoadSnapshots()
		})
	})
}
