package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerproxy"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// sandboxEngine is the in-app Docker Engine API (*engine.Engine). Tests may
// substitute a fake to verify the switch's Open/Close/SetLocal lifecycle.
type sandboxEngine interface {
	Open(ctx context.Context) error
	Close(timeout time.Duration) error
	Handler() http.Handler
	RunningCount() int
	StopRunning(timeout time.Duration)
	VolumeOwnership() (owned map[string]bool, users map[string][]string)
	BuilderSocket() string
	BuilderState() engine.BuilderState
}

// initDockerProxy constructs the Model's Docker API proxy and in-app sandbox
// engine, and keeps atomic snapshots of the Docker Engine spec (for request-time
// bind-mount checks) and saved sandbox images (for engine image pruning). While
// the engine sandbox is active, the snapshot reflects the spec it started with;
// otherwise it falls back to the saved spec. Call it from NewModel after m.sup
// and m.Rows are initialized.
func (m *Model) initDockerProxy(dataDir string) {
	ggui.Watch(m.Rows, func(rows []Row) {
		m.syncEngineSpec(rows)
	})
	ggui.Watch(m.Specs, func(specs []sandbox.Spec) {
		m.syncKeepImages(specs)
	})
	m.syncKeepImages(ggui.Peek(m.Specs))
	m.dockerProxy = dockerproxy.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", recipes.DockerBackendSocket())
	})
	m.dockerProxy.Rewrite = m.rewriteContainerCreate
	m.dockerProxy.Check = m.checkContainerCreate
	m.dockerProxy.Changed = func() {
		m.post(func() {
			m.refreshEngineRunning()
			m.refreshEngineVolumes()
			m.refreshBuilderState()
			m.LoadDocker()
			m.LoadDockerImages()
		})
	}
	engineDir := filepath.Join(dataDir, "engine")
	m.sandboxEngine = engine.New(engine.Config{
		Dir:              engineDir,
		Runner:           engine.NewRunner(m.Client, engineDir),
		Version:          AppVersion,
		Changed:          m.dockerProxy.NotifyChanged,
		KeepImages:       m.sandboxKeepImages,
		BuilderSocketDir: recipes.EngineSocketDir(),
	})
	ggui.OnCleanup(m.CloseDockerProxy)
}

func (m *Model) syncKeepImages(specs []sandbox.Spec) {
	imgs := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Image != "" {
			imgs = append(imgs, s.Image)
		}
	}
	m.keepImages.Store(&imgs)
}

func (m *Model) sandboxKeepImages() []string {
	if p := m.keepImages.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

// SetSandboxEngine turns the experimental in-app sandbox engine on or off.
// Turning it on routes the Docker API proxy to the engine handler immediately
// (answering 503 until Open completes) and opens the engine asynchronously;
// turning it off routes the proxy back to the Docker Engine sandbox immediately
// and stops the engine's containers asynchronously.
func (m *Model) SetSandboxEngine(on bool) {
	if ggui.Peek(m.Settings).SandboxEngine == on {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = on })
	if m.Offline {
		return
	}
	if on {
		m.openSandboxEngine()
	} else {
		m.closeSandboxEngine()
	}
}

func (m *Model) openSandboxEngine() {
	if m.Offline || m.sandboxEngine == nil || m.dockerProxy == nil {
		return
	}
	m.sandboxEngineGen++
	gen := m.sandboxEngineGen
	eng := m.sandboxEngine
	set := ggui.Peek(m.Settings)
	run := m.runCLI
	sockPath := eng.BuilderSocket()
	dockerHost := m.dockerHost()
	// Route traffic to the in-app engine right away so requests and LoadDocker
	// never hit the dind backend while Open is in flight; Handler answers 503
	// until Open finishes.
	m.dockerProxy.SetLocal(eng.Handler())
	var (
		bxStatus         BuildxStatus
		bxChecked        bool
		bxPrevSave       string
		bxSavePrev       bool
		bxInstalledAlias bool
	)
	m.async(func(ctx context.Context) error {
		// Configure buildx before eng.Open binds buildkit.sock so buildx
		// inspect/create probes do not lazily launch buildkitd at startup.
		if sockPath != "" {
			bxChecked = true
			if cli, ok := findCLI("docker", set.DockerPath); !ok {
				bxStatus = BuildxStatus{Checked: true, NoCLI: true}
			} else {
				bxCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				bxStatus, bxPrevSave, bxSavePrev, bxInstalledAlias = configureBuildx(bxCtx, cli, run, sockPath, dockerHost, set.DockerBuilderBefore)
				cancel()
			}
		}
		return eng.Open(ctx)
	}, func(err error) {
		if (bxSavePrev && bxPrevSave != "") || bxInstalledAlias {
			m.UpdateSettings(func(s *store.Settings) {
				if bxSavePrev && bxPrevSave != "" {
					s.DockerBuilderBefore = bxPrevSave
				}
				if bxInstalledAlias {
					s.DockerBuilderAliasInstalled = true
				}
			})
		}
		if !ggui.Peek(m.Settings).SandboxEngine {
			// Setting was turned off while Open was in flight: make sure the
			// engine is closed if Open succeeded and undo any buildx changes.
			if err == nil {
				timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
				go func() { _ = eng.Close(timeout) }()
			}
			if bxInstalledAlias || (bxSavePrev && bxPrevSave != "") {
				m.syncSandboxBuilder(false)
			}
			return
		}
		if m.sandboxEngineGen != gen {
			// Setting is still on under a newer open generation; do not close
			// the engine or surface stale errors.
			return
		}
		if err != nil {
			m.toastError(tr("Could not start the sandbox engine"), err)
			return
		}
		if bxChecked {
			m.BuildxStatus.Set(bxStatus)
		}
		m.dockerProxy.SetLocal(eng.Handler())
		m.refreshEngineRunning()
		m.refreshEngineVolumes()
		m.refreshBuilderState()
		m.LoadDocker()
		m.LoadDockerImages()
	})
}

func (m *Model) closeSandboxEngine() {
	if m.dockerProxy != nil {
		m.dockerProxy.SetLocal(nil)
	}
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.sandboxEngineGen++
	eng := m.sandboxEngine
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	m.async(func(context.Context) error {
		return eng.Close(timeout)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not stop the sandbox engine"), err)
		}
		// Restore buildx after eng.Close removes buildkit.sock so buildx
		// inspect does not wake buildkitd while shutting down.
		if !ggui.Peek(m.Settings).SandboxEngine {
			m.syncSandboxBuilder(false)
		}
		m.refreshEngineRunning()
		m.refreshEngineVolumes()
		m.refreshBuilderState()
		m.LoadDocker()
		m.LoadDockerImages()
	})
}

// CloseSandboxEngine detaches the proxy from the in-app engine and stops all
// running engine containers synchronously. Called on app quit.
func (m *Model) CloseSandboxEngine(timeout time.Duration) {
	if m.dockerProxy != nil {
		m.dockerProxy.SetLocal(nil)
	}
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	_ = m.sandboxEngine.Close(timeout)
}

// StopSandboxEngineContainers stops all running containers in the in-app
// sandbox engine while keeping the engine open.
func (m *Model) StopSandboxEngineContainers(timeout time.Duration) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.sandboxEngine.StopRunning(timeout)
}

func (m *Model) sandboxEngineRunningCount() int {
	if m.Offline || m.sandboxEngine == nil {
		return 0
	}
	return m.sandboxEngine.RunningCount()
}

func (m *Model) refreshEngineRunning() {
	m.EngineRunning.Set(m.sandboxEngineRunningCount())
}

func (m *Model) refreshBuilderState() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.BuilderState.Set(m.sandboxEngine.BuilderState())
}

// ActiveCount reports the total number of running user sandboxes plus running
// experimental sandbox-engine containers.
func (m *Model) ActiveCount() int {
	return len(m.sup.Running()) + m.sandboxEngineRunningCount()
}

func (m *Model) sandboxEngineVolumeOwnership() (map[string]bool, map[string][]string) {
	if m.sandboxEngine == nil {
		return nil, nil
	}
	return m.sandboxEngine.VolumeOwnership()
}

func (m *Model) refreshEngineVolumes() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	eng := m.sandboxEngine
	var users map[string][]string
	m.async(func(context.Context) error {
		_, users = eng.VolumeOwnership()
		return nil
	}, func(error) {
		m.EngineVolumeUsers.Set(users)
	})
}

func (m *Model) syncEngineSpec(rows []Row) {
	for _, r := range rows {
		if r.Spec.Recipe != recipes.Docker {
			continue
		}
		if m.sup != nil {
			if active, ok := m.sup.ActiveSpec(r.Spec.ID); ok {
				m.engineSpec.Store(&active)
				return
			}
		}
		c := r.Spec.Clone()
		m.engineSpec.Store(&c)
		return
	}
	m.engineSpec.Store(nil)
}

// startDockerProxy binds the Docker API proxy listener for the configured
// endpoint. It is skipped when Offline is set. When the experimental sandbox
// engine setting is on, the proxy routes to its handler immediately before
// listening so requests never leak to the dind backend while Open finishes.
func (m *Model) startDockerProxy() {
	if m.Offline || m.dockerProxy == nil || m.dockerProxyStarted {
		return
	}
	m.dockerProxyStarted = true
	if ggui.Peek(m.Settings).SandboxEngine && m.sandboxEngine != nil {
		m.dockerProxy.SetLocal(m.sandboxEngine.Handler())
	}
	m.bindDockerProxy(m.DockerAPI())
}

// bindDockerProxy binds or moves the Docker API proxy listener to api and
// records any listener error in DockerProxyErr and a warning toast.
func (m *Model) bindDockerProxy(api recipes.DockerAPI) {
	if m.Offline || m.dockerProxy == nil {
		return
	}
	network, addr := api.ListenAddr()
	if err := m.dockerProxy.Listen(network, addr); err != nil {
		m.DockerProxyErr.Set(err.Error())
		m.Toasts.Push(ui.Toast(tr("Could not listen for Docker"), err.Error()).Warning().Duration(8 * time.Second))
		return
	}
	m.DockerProxyErr.Set("")
}

// CloseDockerProxy stops the Docker API proxy listener and closes active
// connections.
func (m *Model) CloseDockerProxy() {
	if m.dockerProxy != nil {
		_ = m.dockerProxy.Close()
	}
}

func (m *Model) checkContainerCreate(_ *http.Request, body []byte) error {
	sp := m.engineSpec.Load()
	if sp == nil {
		return nil
	}
	spec := *sp
	if m.sup != nil {
		if active, ok := m.sup.ActiveSpec(spec.ID); ok {
			spec = active
		}
	}
	return checkCreateBinds(body, spec)
}

// checkCreateBinds rejects a POST /containers/create body that bind-mounts a
// host path outside the folders shared with engine. Unparsable bodies pass
// through untouched so dockerd returns its own error.
func checkCreateBinds(body []byte, engine sandbox.Spec) error {
	var req struct {
		HostConfig *struct {
			Binds  []string `json:"Binds"`
			Mounts []struct {
				Type   string `json:"Type"`
				Source string `json:"Source"`
			} `json:"Mounts"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.HostConfig == nil {
		return nil
	}
	for _, b := range req.HostConfig.Binds {
		src, _, ok := strings.Cut(b, ":")
		if !ok || !strings.HasPrefix(src, "/") {
			continue
		}
		if recipes.UnsharedBind(src, engine) {
			return unsharedBindError(src)
		}
	}
	for _, m := range req.HostConfig.Mounts {
		if !strings.EqualFold(m.Type, "bind") || !strings.HasPrefix(m.Source, "/") {
			continue
		}
		if recipes.UnsharedBind(m.Source, engine) {
			return unsharedBindError(m.Source)
		}
	}
	return nil
}

func unsharedBindError(path string) error {
	return fmt.Errorf("%s is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again.", path)
}

// rewriteContainerCreate changes a container's creation on its way to the
// engine: on Windows the folders docker names the Windows way are named as
// the engine has them.
func (m *Model) rewriteContainerCreate(_ *http.Request, body []byte) []byte {
	if runtime.GOOS != "windows" {
		return nil
	}
	return windowsBinds(body)
}

// windowsSource splits a bind at the colon after its source, C:\\src:/dst
// into C:\\src and :/dst, the drive's colon aside; whether the source is a
// Windows folder is recipes.WindowsGuestPath's to say.
func windowsSource(bind string) (source, rest string) {
	if len(bind) > 2 {
		if i := strings.IndexByte(bind[2:], ':'); i >= 0 {
			return bind[:2+i], bind[2+i:]
		}
	}
	return bind, ""
}

// windowsBinds returns a POST /containers/create body with the bind mount
// sources written the Windows way, C:\Users\me\src, as the engine has
// them, /c/Users/me/src; nil when there is none, or the body is not what
// it should be, which is dockerd's to say.
func windowsBinds(body []byte) []byte {
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // a memory limit must come out as it went in
	if err := dec.Decode(&req); err != nil {
		return nil
	}
	host, _ := req["HostConfig"].(map[string]any)
	if host == nil {
		return nil
	}
	changed := false
	if binds, _ := host["Binds"].([]any); binds != nil {
		for i, b := range binds {
			s, _ := b.(string)
			src, rest := windowsSource(s)
			if g, err := recipes.WindowsGuestPath(src); err == nil {
				binds[i], changed = g+rest, true
			}
		}
	}
	if mounts, _ := host["Mounts"].([]any); mounts != nil {
		for _, mt := range mounts {
			mount, _ := mt.(map[string]any)
			if typ, _ := mount["Type"].(string); !strings.EqualFold(typ, "bind") {
				continue
			}
			src, _ := mount["Source"].(string)
			if g, err := recipes.WindowsGuestPath(src); err == nil {
				mount["Source"], changed = g, true
			}
		}
	}
	if !changed {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}
