package ui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ironpark/ggui"

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
}

// Equal makes BuildxStatus comparable for signals.
func (s BuildxStatus) Equal(o BuildxStatus) bool { return s == o }

// buildxInspectInfo holds the fields parsed from `docker buildx inspect`.
type buildxInspectInfo struct {
	Name        string
	Driver      string
	Endpoint    string
	DefaultLoad bool
}

func parseBuildxInspect(out []byte) buildxInspectInfo {
	var info buildxInspectInfo
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
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

func buildxEndpointForSocket(sockPath string) string {
	return "unix://" + filepath.ToSlash(sockPath)
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
	status.Checked = true
	if cli == "" {
		status.NoCLI = true
		return status, "", false, false
	}
	if _, err := run(ctx, cli, "buildx", "version"); err != nil {
		status.NoBuildx = true
		return status, "", false, false
	}

	wantEndpoint := buildxEndpointForSocket(sockPath)
	needCreate := true
	if out, err := run(ctx, cli, "buildx", "inspect", engine.BuilderName); err == nil {
		info := parseBuildxInspect(out)
		if info.Driver == "remote" && sameEndpoint(info.Endpoint, wantEndpoint) && info.DefaultLoad {
			needCreate = false
		} else {
			// Only remove our own gvx-desktop builder if its config is stale.
			if _, err := run(ctx, cli, "buildx", "rm", engine.BuilderName); err != nil {
				status.Err = firstLine(err.Error())
				return status, "", false, false
			}
		}
	}
	if needCreate {
		if _, err := run(ctx, cli, "buildx", "create",
			"--name", engine.BuilderName,
			"--driver", "remote",
			"--driver-opt", "default-load=true",
			wantEndpoint,
		); err != nil {
			status.Err = firstLine(err.Error())
			return status, "", false, false
		}
	}
	status.Ready = true

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
	// the sandbox engine off can restore it.
	curOut, _ := run(ctx, cli, "--context", DockerContextName, "buildx", "inspect")
	curName := parseBuildxInspect(curOut).Name
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
	dockerHost := m.dockerHost()

	if sockPath == "" {
		return
	}
	if !on {
		m.BuildxStatus.Set(BuildxStatus{})
		m.async(func(ctx context.Context) error {
			cli, ok := findCLI("docker", set.DockerPath)
			if !ok {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			clearPrev, clearAlias := restoreBuildx(ctx, cli, run, dockerHost, set.DockerBuilderBefore, set.DockerBuilderAliasInstalled)
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
		st, prevSave, savePrev, installedAlias = configureBuildx(ctx, cli, run, sockPath, dockerHost, set.DockerBuilderBefore)
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
