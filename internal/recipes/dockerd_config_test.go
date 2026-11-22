package recipes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeDockerConfig(t *testing.T) {
	spec := DockerEngine()
	out, err := NormalizeDockerConfig(spec, nil)
	if err != nil || !strings.Contains(string(out), `"host.docker.internal:5001"`) {
		t.Fatalf("empty: %s %v", out, err)
	}
	// The user's own file keeps its layout when the registry is in it.
	mine := []byte("{\n  \"debug\": true,\n  \"insecure-registries\": [\"host.docker.internal:5001\", \"nas:5000\"]\n}\n")
	if out, _ := NormalizeDockerConfig(spec, mine); string(out) != string(mine) {
		t.Errorf("rewritten: %s", out)
	}
	// One without it gets it, and keeps its other registries and settings.
	out, err = NormalizeDockerConfig(spec, []byte(`{"debug":true,"insecure-registries":["nas:5000"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Debug bool     `json:"debug"`
		Regs  []string `json:"insecure-registries"`
	}
	_ = json.Unmarshal(out, &cfg)
	if !cfg.Debug || !slices.Equal(cfg.Regs, []string{"nas:5000", "host.docker.internal:5001"}) {
		t.Errorf("added: %s", out)
	}
	// A command line that names a registry keeps the file out of it.
	flagged := spec.Clone()
	flagged.Command = "--tls=false --insecure-registry=host.docker.internal:5001"
	if out, _ := NormalizeDockerConfig(flagged, nil); strings.TrimSpace(string(out)) != "{}" {
		t.Errorf("flagged: %s", out)
	}
	for _, bad := range []string{"{", "[1]", "null", `{"insecure-registries":"x"}`} {
		if _, err := NormalizeDockerConfig(spec, []byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestDockerConfigChanges(t *testing.T) {
	before := []byte(`{"debug":false,"insecure-registries":["a"],"data-root":"/x"}`)
	changed, restart, err := DockerConfigChanges(before, []byte(`{"debug":true, "insecure-registries":["a"], "data-root":"/x", "registry-mirrors":["https://m"]}`))
	if err != nil || !slices.Equal(changed, []string{"debug", "registry-mirrors"}) || len(restart) != 0 {
		t.Errorf("reloadable: %v %v %v", changed, restart, err)
	}
	changed, restart, _ = DockerConfigChanges(before, []byte(`{"debug":false,"insecure-registries":["a"]}`))
	if !slices.Equal(changed, []string{"data-root"}) || !slices.Equal(restart, []string{"data-root"}) {
		t.Errorf("restart: %v %v", changed, restart)
	}
	// Layout is not a change.
	if changed, _, _ := DockerConfigChanges(before, []byte("{ \"data-root\": \"/x\",\n \"insecure-registries\": [ \"a\" ], \"debug\": false }")); len(changed) != 0 {
		t.Errorf("layout: %v", changed)
	}
	if _, _, err := DockerConfigChanges(before, []byte("{")); err == nil {
		t.Error("invalid JSON")
	}
}

// Prepare writes the engine's daemon.json in ~/.gvx/docker, the folder the
// sandbox has at /etc/docker, keeping what the user wrote there.
func TestPrepareDockerConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	spec := DockerEngine()
	if err := Prepare(spec); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".gvx", "docker", "daemon.json")
	if DockerConfigFile() != file {
		t.Fatalf("file %s", DockerConfigFile())
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), RegistryHost()) {
		t.Fatalf("written: %s %v", data, err)
	}
	mine := "{\"debug\": true, \"insecure-registries\": [\"host.docker.internal:5001\"]}\n"
	if _, err := WriteDockerConfig(spec, []byte(mine)); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(spec); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); string(data) != mine {
		t.Errorf("rewritten: %s", data)
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".gvx", "docker", ".daemon.json-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	// A broken file stops the start, saying where it is.
	_ = os.WriteFile(file, []byte("{"), 0o644)
	if err := Prepare(spec); err == nil || !strings.Contains(err.Error(), file) {
		t.Errorf("broken: %v", err)
	}
	// A sandbox without the folder mounted is left alone.
	old := spec.Clone()
	old.Mounts = slices.DeleteFunc(old.Mounts, isDockerConfigMount)
	if err := Prepare(old); err != nil {
		t.Errorf("old: %v", err)
	}
}
