package recipes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// The Docker Engine's dockerd reads its settings from /etc/docker/daemon.json,
// as dockerd does anywhere: the folder is a folder of this computer, shared
// read-only, so the app writes the file and dockerd reads it again on SIGHUP
// without a restart.
const (
	dockerConfigDirPath = "~/.gvx/docker"
	dockerConfigGuest   = "/etc/docker"
	dockerConfigName    = "daemon.json"
)

// DockerConfigFile is the host path of the engine's daemon.json.
func DockerConfigFile() string {
	return filepath.Join(rungv.ExpandHome(dockerConfigDirPath), dockerConfigName)
}

// dockerConfigMount is the mount that gives the engine the folder.
func dockerConfigMount() sandbox.Mount {
	return sandbox.Mount{Type: sandbox.MountBind, Source: dockerConfigDirPath, Target: dockerConfigGuest, ReadOnly: true}
}

// HasDockerConfig reports whether the engine's sandbox reads daemon.json
// from this computer: whether it has the folder mounted. One saved by an
// earlier version of the app has it from its next start (Upgrade).
func HasDockerConfig(spec sandbox.Spec) bool {
	return slices.ContainsFunc(spec.Mounts, isDockerConfigMount)
}

func isDockerConfigMount(m sandbox.Mount) bool {
	return m.Type == sandbox.MountBind && m.Target == dockerConfigGuest &&
		filepath.Clean(rungv.ExpandHome(m.Source)) == filepath.Clean(rungv.ExpandHome(dockerConfigDirPath))
}

// insecureRegistryFlagged reports whether the engine's command line names
// an insecure registry: dockerd then refuses insecure-registries in the
// file, so the file leaves them to the flags.
func insecureRegistryFlagged(spec sandbox.Spec) bool {
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return false
	}
	_, ok := flagValue(args, "--insecure-registry")
	return ok
}

// ParseDockerConfig reads a daemon.json: a JSON object, or nothing at all.
func ParseDockerConfig(data []byte) (map[string]json.RawMessage, error) {
	return parseDockerConfig[json.RawMessage](data)
}

func parseDockerConfig[V any](data []byte) (map[string]V, error) {
	cfg := map[string]V{}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("the file holds null, not an object")
	}
	return cfg, nil
}

// NormalizeDockerConfig is a daemon.json as the engine's sandbox needs it:
// with the local image registry among its insecure registries, unless
// the command line names them. It returns data itself when nothing is
// missing, so the user's own layout stays.
func NormalizeDockerConfig(spec sandbox.Spec, data []byte) ([]byte, error) {
	cfg, err := ParseDockerConfig(data)
	if err != nil {
		return nil, err
	}
	if insecureRegistryFlagged(spec) {
		if len(bytes.TrimSpace(data)) == 0 {
			return []byte("{}\n"), nil
		}
		return data, nil
	}
	var regs []string
	if raw, ok := cfg["insecure-registries"]; ok {
		if err := json.Unmarshal(raw, &regs); err != nil {
			return nil, errors.New("insecure-registries must be a list of strings")
		}
	}
	if slices.Contains(regs, RegistryHost()) {
		return data, nil
	}
	raw, _ := json.Marshal(append(regs, RegistryHost()))
	cfg["insecure-registries"] = raw
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ReadDockerConfig is the engine's daemon.json as it is on this computer,
// nothing when there is none yet.
func ReadDockerConfig() ([]byte, error) {
	data, err := os.ReadFile(DockerConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// WriteDockerConfig saves the engine's daemon.json, normalized for spec,
// and returns what it wrote. It replaces the file whole, so dockerd never
// reads half of it.
func WriteDockerConfig(spec sandbox.Spec, data []byte) ([]byte, error) {
	out, err := NormalizeDockerConfig(spec, data)
	if err != nil {
		return nil, err
	}
	file := DockerConfigFile()
	if have, err := os.ReadFile(file); err == nil && bytes.Equal(have, out) {
		return out, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".daemon.json-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return nil, err
	}
	return out, nil
}

// prepareDockerConfig writes the daemon.json an engine sandbox with the
// folder mounted reads as it starts.
func prepareDockerConfig(spec sandbox.Spec) error {
	if !HasDockerConfig(spec) {
		return nil
	}
	data, err := ReadDockerConfig()
	if err != nil {
		return err
	}
	if _, err := WriteDockerConfig(spec, data); err != nil {
		return fmt.Errorf("%s: %w", DockerConfigFile(), err)
	}
	return nil
}

// dockerReloadable are the daemon.json settings dockerd applies on SIGHUP
// (dockerd's documentation, "Configuration reload behavior"); any other
// change takes a restart of the engine.
var dockerReloadable = map[string]bool{
	"debug":                    true,
	"labels":                   true,
	"live-restore":             true,
	"max-concurrent-downloads": true,
	"max-concurrent-uploads":   true,
	"max-download-attempts":    true,
	"default-runtime":          true,
	"runtimes":                 true,
	"authorization-plugins":    true,
	"insecure-registries":      true,
	"registry-mirrors":         true,
	"shutdown-timeout":         true,
	"features":                 true,
}

// DockerConfigChanges are the settings that differ between two
// daemon.json files, and those of them dockerd applies only as it starts.
// A broken file before counts as having none.
func DockerConfigChanges(before, after []byte) (changed, needRestart []string, err error) {
	a, _ := parseDockerConfig[any](before)
	b, err := parseDockerConfig[any](after)
	if err != nil {
		return nil, nil, err
	}
	keys := maps.Clone(b)
	for k, v := range a {
		keys[k] = v
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if reflect.DeepEqual(a[k], b[k]) {
			continue
		}
		changed = append(changed, k)
		if !dockerReloadable[k] {
			needRestart = append(needRestart, k)
		}
	}
	return changed, needRestart, nil
}
