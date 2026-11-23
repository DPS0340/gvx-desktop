package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockercli"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// BuildxStatus describes how docker buildx is configured for the experimental
// sandbox engine's gvx-desktop remote builder.
type BuildxStatus struct {
	Checked  bool   // configuration check has run
	Busy     bool   // configuration update in flight
	Ready    bool   // the gvx-desktop builder exists with the right endpoint
	Selected bool   // gvx-desktop is the active builder for the gvx docker context
	NoCLI    bool   // docker CLI was not found
	NoBuildx bool   // docker buildx plugin is not installed
	NoCtx    bool   // gvx docker context does not exist yet
	Err      string // short failure message when configuration failed
	// Amd64Node is set when the gvx-desktop builder has the amd64 node.
	Amd64Node bool
}

// Equal makes BuildxStatus comparable for signals.
func (s BuildxStatus) Equal(o BuildxStatus) bool { return s == o }

// buildxNodeInfo holds one node parsed from `docker buildx inspect` or a
// buildx instance JSON file.
type buildxNodeInfo struct {
	Name           string
	Endpoint       string
	DefaultLoad    bool
	Platforms      []string
	FixedPlatforms []string // platforms explicitly pinned with '*'
}

// buildxInspectInfo holds the fields parsed from `docker buildx inspect` or a
// buildx instance JSON file.
type buildxInspectInfo struct {
	Name        string
	Driver      string
	Endpoint    string
	DefaultLoad bool
	Nodes       []buildxNodeInfo
}

func (info buildxInspectInfo) findNode(name string) (buildxNodeInfo, bool) {
	for _, n := range info.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return buildxNodeInfo{}, false
}

func parseBuildxInspect(out []byte) buildxInspectInfo {
	var info buildxInspectInfo
	inNodes := false
	curIdx := -1
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line == "Nodes:" {
			inNodes = true
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if inNodes {
			switch key {
			case "Name":
				info.Nodes = append(info.Nodes, buildxNodeInfo{Name: val})
				curIdx = len(info.Nodes) - 1
			case "Endpoint":
				if curIdx >= 0 {
					info.Nodes[curIdx].Endpoint = val
				}
				if info.Endpoint == "" {
					info.Endpoint = val
				}
			case "Driver Options":
				dl := strings.Contains(val, `default-load="true"`) || strings.Contains(val, "default-load=true")
				if curIdx >= 0 && dl {
					info.Nodes[curIdx].DefaultLoad = true
				}
				if curIdx <= 0 && dl {
					info.DefaultLoad = true
				}
			case "Platforms":
				if curIdx >= 0 {
					for _, part := range strings.Split(val, ",") {
						p := strings.TrimSpace(part)
						if p == "" {
							continue
						}
						fixed := strings.HasSuffix(p, "*")
						p = strings.TrimSuffix(p, "*")
						p = strings.TrimSpace(p)
						if p == "" {
							continue
						}
						info.Nodes[curIdx].Platforms = append(info.Nodes[curIdx].Platforms, p)
						if fixed {
							info.Nodes[curIdx].FixedPlatforms = append(info.Nodes[curIdx].FixedPlatforms, p)
						}
					}
				}
			}
			continue
		}
		switch key {
		case "Name":
			if info.Name == "" {
				info.Name = val
			}
		case "Driver":
			if info.Driver == "" {
				info.Driver = val
			}
		case "Endpoint":
			if info.Endpoint == "" {
				info.Endpoint = val
			}
		case "Driver Options":
			if strings.Contains(val, `default-load="true"`) || strings.Contains(val, "default-load=true") {
				info.DefaultLoad = true
			}
		}
	}
	return info
}

func nodeMatchesAmd64(n buildxNodeInfo, wantEndpoint string) bool {
	if n.Name != engine.Amd64NodeName || !n.DefaultLoad || !sameEndpoint(n.Endpoint, wantEndpoint) {
		return false
	}
	plats := n.FixedPlatforms
	if len(plats) == 0 {
		plats = n.Platforms
	}
	return slices.Equal(plats, []string{engine.Amd64Platform})
}

func buildxEndpointForSocket(sockPath string) string {
	return "unix://" + filepath.ToSlash(sockPath)
}

// buildxConfigDir returns the directory where buildx stores instances and
// per-context defaults, honoring $BUILDX_CONFIG and $DOCKER_CONFIG.
func buildxConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("BUILDX_CONFIG")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "buildx")
	}
	return filepath.Join(rungv.ExpandHome("~/.docker"), "buildx")
}

// readBuildxInstance reads a buildx builder's configuration directly from disk
// without opening a gRPC connection to any node socket (probe-safe).
func readBuildxInstance(name string) (buildxInspectInfo, bool) {
	b, err := os.ReadFile(filepath.Join(buildxConfigDir(), "instances", name))
	if err != nil {
		return buildxInspectInfo{}, false
	}
	var raw struct {
		Name   string `json:"Name"`
		Driver string `json:"Driver"`
		Nodes  []struct {
			Name       string            `json:"Name"`
			Endpoint   string            `json:"Endpoint"`
			DriverOpts map[string]string `json:"DriverOpts"`
			Platforms  []struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"Platforms"`
		} `json:"Nodes"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return buildxInspectInfo{}, false
	}
	info := buildxInspectInfo{
		Name:   raw.Name,
		Driver: raw.Driver,
	}
	for i, n := range raw.Nodes {
		dl := strings.EqualFold(strings.TrimSpace(n.DriverOpts["default-load"]), "true")
		var plats []string
		for _, p := range n.Platforms {
			s := p.OS
			if p.Architecture != "" {
				s += "/" + p.Architecture
			}
			if p.Variant != "" {
				s += "/" + p.Variant
			}
			if s != "" {
				plats = append(plats, s)
			}
		}
		node := buildxNodeInfo{
			Name:           n.Name,
			Endpoint:       n.Endpoint,
			DefaultLoad:    dl,
			Platforms:      plats,
			FixedPlatforms: slices.Clone(plats),
		}
		info.Nodes = append(info.Nodes, node)
		if i == 0 {
			info.Endpoint = n.Endpoint
			info.DefaultLoad = dl
		}
	}
	return info, true
}

// readBuildxDefaultForContext reads the configured default builder name for a
// docker context from buildx's config files without running `buildx inspect`.
func readBuildxDefaultForContext(ctxName string) string {
	sum := sha256.Sum256([]byte(ctxName))
	key := hex.EncodeToString(sum[:])
	for _, p := range []string{
		filepath.Join(buildxConfigDir(), "defaults", key),
		filepath.Join(buildxConfigDir(), "current"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec struct {
			Key  string `json:"Key"`
			Name string `json:"Name"`
		}
		if json.Unmarshal(b, &rec) == nil && rec.Name != "" {
			if strings.HasSuffix(p, "current") && rec.Key != "" && rec.Key != ctxName {
				continue
			}
			return rec.Name
		}
	}
	return ""
}

// dockerConfigPath returns the path to Docker's config.json, honoring
// $DOCKER_CONFIG when set and otherwise falling back to ~/.docker/config.json.
func dockerConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	return filepath.Join(rungv.ExpandHome("~/.docker"), "config.json")
}

// dockerBuilderAlias reads aliases.builder from Docker's config.json, or ""
// when the file or field is absent.
func dockerBuilderAlias(configPath string) string {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Aliases map[string]string `json:"aliases"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Aliases["builder"])
}

// configureBuildx ensures the engine-managed gvx-desktop remote builder exists
// with default-load=true pointing at sockPath, enables the buildx builder alias
// (recording whether the app installed it so switch-off can undo it), sets the
// per-endpoint default for dockerHost (used by `docker compose build` and
// `DOCKER_HOST`), and selects gvx-desktop as the default builder for the gvx
// docker context without affecting any other context or builder.
func configureBuildx(ctx context.Context, cli string, run cliRunner, sockPath, dockerHost, prevRecorded string) (status BuildxStatus, prevToSave string, savePrev bool, installedAlias bool) {
	return configureBuildxWithAmd64(ctx, cli, run, sockPath, dockerHost, prevRecorded, false, "", false)
}

// configureBuildxWithAmd64 configures gvx-desktop and reconciles its optional
// linux/amd64 node. When probeSafe is true (e.g. UseSandboxBuilder while the
// engine is already open and sockets are bound), it reads buildx's instance
// JSON directly instead of running `buildx inspect` so it never connects to
// the lazy front sockets or wakes the amd64 VM / triggers asset downloads.
func configureBuildxWithAmd64(
	ctx context.Context,
	cli string,
	run cliRunner,
	sockPath, dockerHost, prevRecorded string,
	amd64On bool,
	amd64Sock string,
	probeSafe bool,
) (status BuildxStatus, prevToSave string, savePrev bool, installedAlias bool) {
	status.Checked = true
	if cli == "" {
		status.NoCLI = true
		return status, "", false, false
	}
	if dockercli.IsBundledPath(cli) {
		_, _ = dockercli.Sync(true)
	}
	if _, err := run(ctx, cli, "buildx", "version"); err != nil {
		if b := dockercli.Bundled(); b.Buildx != "" {
			_, _ = dockercli.Sync(true)
			_, err = run(ctx, cli, "buildx", "version")
		}
		if err != nil {
			status.NoBuildx = true
			return status, "", false, false
		}
	}

	wantEndpoint := buildxEndpointForSocket(sockPath)
	needCreate := true
	var (
		prevAmd64 buildxNodeInfo
		hadAmd64  bool
	)

	var (
		info    buildxInspectInfo
		hasInfo bool
	)
	if probeSafe {
		info, hasInfo = readBuildxInstance(engine.BuilderName)
	} else if out, err := run(ctx, cli, "buildx", "inspect", engine.BuilderName); err == nil {
		info = parseBuildxInspect(out)
		hasInfo = true
	}

	if hasInfo {
		prevAmd64, hadAmd64 = info.findNode(engine.Amd64NodeName)
		if info.Driver == "remote" && sameEndpoint(info.Endpoint, wantEndpoint) && info.DefaultLoad {
			needCreate = false
		} else {
			// Only remove our own gvx-desktop builder if its config is stale.
			if _, err := run(ctx, cli, "buildx", "rm", engine.BuilderName); err != nil {
				status.Err = firstLine(err.Error())
				return status, "", false, false
			}
			hadAmd64 = false
		}
	}
	if needCreate {
		if _, err := run(ctx, cli, "buildx", "create",
			"--name", engine.BuilderName,
			"--driver", "remote",
			"--driver-opt", "default-load=true",
			wantEndpoint,
		); err != nil {
			// In probeSafe mode, if an instance existed outside buildxConfigDir,
			// remove and recreate it.
			if probeSafe && strings.Contains(strings.ToLower(err.Error()), "existing instance") {
				if _, rmErr := run(ctx, cli, "buildx", "rm", engine.BuilderName); rmErr == nil {
					_, err = run(ctx, cli, "buildx", "create",
						"--name", engine.BuilderName,
						"--driver", "remote",
						"--driver-opt", "default-load=true",
						wantEndpoint,
					)
				}
			}
			if err != nil {
				status.Err = firstLine(err.Error())
				return status, "", false, false
			}
		}
		hadAmd64 = false
	}
	status.Ready = true

	// Reconcile the optional linux/amd64 node: validate an existing node with
	// nodeMatchesAmd64 when the feature is on, and --leave any stale node when
	// the feature is off (e.g. left over after a crash).
	if amd64On && strings.TrimSpace(amd64Sock) != "" {
		wantAmd64EP := buildxEndpointForSocket(amd64Sock)
		if hadAmd64 && nodeMatchesAmd64(prevAmd64, wantAmd64EP) {
			status.Amd64Node = true
		} else if present, err := configureAmd64Node(ctx, cli, run, amd64Sock, true); err == nil {
			status.Amd64Node = present
		} else if status.Err == "" {
			status.Err = firstLine(err.Error())
		}
	} else if hadAmd64 {
		_, _ = configureAmd64Node(ctx, cli, run, "", false)
		status.Amd64Node = false
	}

	// Route `docker build` through buildx so it honors per-context default
	// builders instead of forcing the daemon's built-in builder. Check first
	// whether the user already had aliases.builder set so we neither overwrite
	// an existing alias nor uninstall one the user configured themselves.
	if dockerBuilderAlias(dockerConfigPath()) == "" {
		if _, err := run(ctx, cli, "buildx", "install"); err == nil {
			installedAlias = true
		}
	}

	// Set the per-endpoint default for dockerHost so `docker compose build`
	// (which invokes buildx bake against the endpoint URL) and DOCKER_HOST
	// sessions select gvx-desktop without touching other endpoints/contexts.
	if dockerHost != "" {
		_, _ = run(ctx, cli, "-H", dockerHost, "buildx", "use", "--default", engine.BuilderName)
	}

	// Only select the builder on the gvx context if that context exists.
	if _, err := run(ctx, cli, "context", "inspect", DockerContextName); err != nil {
		status.NoCtx = true
		return status, "", false, installedAlias
	}

	// Read the current builder for the gvx context before switching so turning
	// the sandbox engine off can restore it. In probeSafe mode avoid `buildx
	// inspect` so selecting gvx-desktop never wakes the lazy VM socket.
	var curName string
	if probeSafe {
		curName = readBuildxDefaultForContext(DockerContextName)
	} else {
		curOut, _ := run(ctx, cli, "--context", DockerContextName, "buildx", "inspect")
		curName = parseBuildxInspect(curOut).Name
	}
	if curName == engine.BuilderName {
		status.Selected = true
		return status, "", false, installedAlias
	}
	if prevRecorded == "" && curName != "" && curName != engine.BuilderName {
		prevToSave = curName
		savePrev = true
	}

	if _, err := run(ctx, cli, "--context", DockerContextName, "buildx", "use", "--default", engine.BuilderName); err != nil {
		status.Err = firstLine(err.Error())
		return status, "", false, installedAlias
	}
	status.Selected = true
	return status, prevToSave, savePrev, installedAlias
}

// configureAmd64Node appends or leaves the linux/amd64 VM node on the
// gvx-desktop buildx builder. It checks buildx's instance JSON first when on is
// true so an already-matching node is not probed by `buildx create --append`.
func configureAmd64Node(ctx context.Context, cli string, run cliRunner, amd64Sock string, on bool) (present bool, err error) {
	if cli == "" {
		return false, nil
	}
	if _, err := run(ctx, cli, "buildx", "version"); err != nil {
		return false, nil
	}
	if !on {
		if _, err := run(ctx, cli, "buildx", "create",
			"--leave",
			"--name", engine.BuilderName,
			"--node", engine.Amd64NodeName,
		); err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "not found") || strings.Contains(msg, "failed to find instance") {
				return false, nil
			}
			return false, err
		}
		return false, nil
	}
	if strings.TrimSpace(amd64Sock) == "" {
		return false, nil
	}
	wantEndpoint := buildxEndpointForSocket(amd64Sock)
	if info, ok := readBuildxInstance(engine.BuilderName); ok {
		if existing, found := info.findNode(engine.Amd64NodeName); found && nodeMatchesAmd64(existing, wantEndpoint) {
			return true, nil
		}
	}
	if _, err := run(ctx, cli, "buildx", "create",
		"--append",
		"--name", engine.BuilderName,
		"--node", engine.Amd64NodeName,
		"--platform", engine.Amd64Platform,
		"--driver-opt", "default-load=true",
		wantEndpoint,
	); err != nil {
		return false, err
	}
	return true, nil
}

// restoreBuildx restores the previous builder for the gvx context, clears the
// per-endpoint default for dockerHost, and undoes `docker buildx install` when
// the sandbox engine turns off (only if the app installed the alias and it
// still points to buildx). Never deletes any builder or touches other contexts.
func restoreBuildx(ctx context.Context, cli string, run cliRunner, dockerHost, prevRecorded string, aliasInstalled bool) (clearPrev, clearAlias bool) {
	if cli == "" {
		return false, false
	}
	if _, err := run(ctx, cli, "buildx", "version"); err != nil {
		return false, false
	}
	if aliasInstalled {
		if dockerBuilderAlias(dockerConfigPath()) == "buildx" {
			if _, err := run(ctx, cli, "buildx", "uninstall"); err == nil {
				clearAlias = true
			}
		} else {
			clearAlias = true
		}
	}
	if dockerHost != "" {
		if out, err := run(ctx, cli, "-H", dockerHost, "buildx", "inspect"); err == nil && parseBuildxInspect(out).Name == engine.BuilderName {
			_, _ = run(ctx, cli, "-H", dockerHost, "buildx", "use", "--default", "default")
		}
	}
	if _, err := run(ctx, cli, "context", "inspect", DockerContextName); err != nil {
		return false, clearAlias
	}
	curOut, _ := run(ctx, cli, "--context", DockerContextName, "buildx", "inspect")
	curName := parseBuildxInspect(curOut).Name
	if curName != engine.BuilderName {
		return prevRecorded != "", clearAlias
	}
	target := strings.TrimSpace(prevRecorded)
	if target == "" || target == engine.BuilderName {
		target = DockerContextName
	}
	if _, err := run(ctx, cli, "--context", DockerContextName, "buildx", "use", "--default", target); err != nil && target != DockerContextName {
		_, _ = run(ctx, cli, "--context", DockerContextName, "buildx", "use", "--default", DockerContextName)
	}
	return true, clearAlias
}

// amd64BuildxMu serializes buildx CLI mutations so rapid toggles execute in
// order without racing on BUILDX_CONFIG. amd64NodeGenMu tracks the latest
// syncAmd64Node generation and desired state per Model without adding fields
// to Model.
var (
	amd64BuildxMu  sync.Mutex
	amd64NodeGenMu sync.Mutex
	amd64NodeGen   = map[weak.Pointer[Model]]uint64{}
	amd64NodeWant  = map[weak.Pointer[Model]]bool{}
)

func nextAmd64NodeSync(m *Model, want bool) uint64 {
	k := weak.Make(m)
	amd64NodeGenMu.Lock()
	defer amd64NodeGenMu.Unlock()
	for wk := range amd64NodeGen {
		if wk.Value() == nil {
			delete(amd64NodeGen, wk)
			delete(amd64NodeWant, wk)
		}
	}
	g := amd64NodeGen[k] + 1
	amd64NodeGen[k] = g
	amd64NodeWant[k] = want
	return g
}

func checkAmd64NodeSync(m *Model, gen uint64) (current bool, want bool) {
	k := weak.Make(m)
	amd64NodeGenMu.Lock()
	defer amd64NodeGenMu.Unlock()
	if amd64NodeGen[k] != gen {
		return false, false
	}
	return true, amd64NodeWant[k]
}

func latestAmd64NodeWant(m *Model, fallback bool) bool {
	k := weak.Make(m)
	amd64NodeGenMu.Lock()
	defer amd64NodeGenMu.Unlock()
	if w, ok := amd64NodeWant[k]; ok {
		return w
	}
	return fallback
}

func (m *Model) amd64Socket() string {
	if m.sandboxEngine == nil {
		return ""
	}
	if a, ok := m.sandboxEngine.(interface{ Amd64Socket() string }); ok {
		return a.Amd64Socket()
	}
	return ""
}

// syncSandboxBuilder configures or restores docker buildx quietly in the
// background, surfacing problems as a short status note in Settings.
func (m *Model) syncSandboxBuilder(on bool) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.buildxSyncGen++
	gen := m.buildxSyncGen
	set := ggui.Peek(m.Settings)
	run := m.runCLI
	sockPath := m.sandboxEngine.BuilderSocket()
	amd64Sock := m.amd64Socket()
	dockerHost := m.dockerHost()

	if sockPath == "" {
		return
	}
	if !on {
		nextAmd64NodeSync(m, false)
		m.BuildxStatus.Set(BuildxStatus{})
		m.async(func(ctx context.Context) error {
			cli, ok := findCLI("docker", set.DockerPath)
			if !ok {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			amd64BuildxMu.Lock()
			_, _ = configureAmd64Node(ctx, cli, run, amd64Sock, false)
			clearPrev, clearAlias := restoreBuildx(ctx, cli, run, dockerHost, set.DockerBuilderBefore, set.DockerBuilderAliasInstalled)
			amd64BuildxMu.Unlock()
			if clearPrev || clearAlias {
				m.post(func() {
					if !ggui.Peek(m.Settings).SandboxEngine {
						m.UpdateSettings(func(s *store.Settings) {
							if clearPrev {
								s.DockerBuilderBefore = ""
							}
							if clearAlias {
								s.DockerBuilderAliasInstalled = false
							}
						})
					}
				})
			}
			return nil
		}, func(error) {})
		return
	}
	amd64Want := set.SandboxEngine && set.Amd64Builds
	nextAmd64NodeSync(m, amd64Want)
	m.BuildxStatus.Update(func(s BuildxStatus) BuildxStatus { s.Busy = true; return s })
	var (
		st             BuildxStatus
		prevSave       string
		savePrev       bool
		installedAlias bool
	)
	m.async(func(ctx context.Context) error {
		cli, ok := findCLI("docker", set.DockerPath)
		if !ok {
			st = BuildxStatus{Checked: true, NoCLI: true}
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		amd64BuildxMu.Lock()
		// Read coherent desired amd64 state recorded on the UI thread under
		// the mutex and use probeSafe so UseSandboxBuilder never connects to
		// buildkit-amd64.sock or wakes the VM / starts an asset download.
		wantAmd64 := latestAmd64NodeWant(m, amd64Want)
		st, prevSave, savePrev, installedAlias = configureBuildxWithAmd64(
			ctx, cli, run, sockPath, dockerHost, set.DockerBuilderBefore,
			wantAmd64, amd64Sock, true,
		)
		amd64BuildxMu.Unlock()
		return nil
	}, func(error) {
		if m.buildxSyncGen != gen || !ggui.Peek(m.Settings).SandboxEngine {
			return
		}
		if (savePrev && prevSave != "") || installedAlias {
			m.UpdateSettings(func(s *store.Settings) {
				if savePrev && prevSave != "" {
					s.DockerBuilderBefore = prevSave
				}
				if installedAlias {
					s.DockerBuilderAliasInstalled = true
				}
			})
		}
		m.BuildxStatus.Set(st)
	})
}

// UseSandboxBuilder re-runs buildx configuration on demand from Settings.
func (m *Model) UseSandboxBuilder() {
	if ggui.Peek(m.Settings).SandboxEngine {
		m.syncSandboxBuilder(true)
	}
}

// syncAmd64Node appends the amd64 builder's node (engine.Amd64NodeName,
// --platform linux/amd64, endpoint unix://<engine Amd64Socket>,
// default-load=true) to the gvx-desktop builder, or with on false removes
// that node alone (docker buildx create --leave). It runs quietly in the
// background like syncSandboxBuilder and reports in BuildxStatus.Amd64Node.
// configureBuildx keeps the node when it has to make the builder anew.
//
// The signature is frozen (architect); the body is workstream C's, which
// owns this file. D calls it when the setting or the sandbox engine
// changes.
func (m *Model) syncAmd64Node(on bool) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	set := ggui.Peek(m.Settings)
	wantInitial := on && set.SandboxEngine && set.Amd64Builds
	gen := nextAmd64NodeSync(m, wantInitial)
	run := m.runCLI
	amd64Sock := m.amd64Socket()
	if wantInitial && amd64Sock == "" {
		return
	}

	var (
		present bool
		errMsg  string
		skipped bool
	)
	m.async(func(ctx context.Context) error {
		cli, ok := findCLI("docker", set.DockerPath)
		if !ok {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		amd64BuildxMu.Lock()
		defer amd64BuildxMu.Unlock()

		// Re-check generation and coherent desired state recorded on the UI
		// thread under the mutex so a fast ON->OFF toggle can never end with
		// the node appended while off.
		current, want := checkAmd64NodeSync(m, gen)
		if !current {
			skipped = true
			return nil
		}
		if want && amd64Sock == "" {
			return nil
		}
		var err error
		present, err = configureAmd64Node(ctx, cli, run, amd64Sock, want)
		if err != nil {
			errMsg = firstLine(err.Error())
		}
		return nil
	}, func(error) {
		if skipped {
			return
		}
		if current, _ := checkAmd64NodeSync(m, gen); !current {
			return
		}
		curSet := ggui.Peek(m.Settings)
		if present && (!curSet.SandboxEngine || !curSet.Amd64Builds) {
			return
		}
		m.BuildxStatus.Update(func(s BuildxStatus) BuildxStatus {
			s.Amd64Node = present
			if errMsg != "" && s.Err == "" {
				s.Err = errMsg
			}
			return s
		})
	})
}
