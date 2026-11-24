package recipes

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Registry is the key of the legacy image registry sandbox recipe.
const Registry = "registry"

// RegistryPort is this computer's port (5001, since 5000 is AirPlay's on macOS)
// used by GVX Desktop's built-in local OCI image registry as well as the legacy
// registry:2 sandbox recipe (which stores its images in the gvx-registry volume).
const (
	RegistryImage     = "registry:2"
	RegistryVolume    = "gvx-registry"
	RegistryPort      = 5001
	registryGuestPort = 5000
)

// A guest reaches this computer's loopback address as host.docker.internal,
// and its own as localhost. So the local registry is
//
//   - host.docker.internal:5001 to the Docker Engine, which pushes to it,
//     and to the cluster's nodes, which pull from it;
//   - localhost:5001 to tools that run on this computer, and to the
//     cluster's nodes too, whose containerd is told that it is the other.
//
// The cluster reaches it over plain HTTP, and the engine is told to push to
// it so.

// RegistryHost is the local registry as sandboxes name it.
func RegistryHost() string { return fmt.Sprintf("host.docker.internal:%d", RegistryPort) }

// RegistryLocal is the local registry as this computer names it.
func RegistryLocal() string { return fmt.Sprintf("localhost:%d", RegistryPort) }

// ImageRegistry builds a spec for the legacy registry:2 sandbox recipe.
// GVX Desktop's built-in local OCI registry now serves the same port (5001),
// so ImageRegistry is omitted from All() and kept for backwards compatibility
// with existing saved registry sandboxes.
func ImageRegistry() sandbox.Spec {
	return sandbox.Spec{
		ID:           sandbox.NewID(),
		Name:         "registry",
		Image:        RegistryImage,
		Ports:        []string{fmt.Sprintf("127.0.0.1:%d:%d", RegistryPort, registryGuestPort)},
		Mounts:       []sandbox.Mount{{Type: sandbox.MountVolume, Source: RegistryVolume, Target: "/var/lib/registry"}},
		Recipe:       Registry,
		ReadyPattern: "listening on",
		CreatedAt:    time.Now(),
	}
}

// insecureRegistry is the dockerd flag that lets the engine push to the
// registry over HTTP.
func insecureRegistry() string { return "--insecure-registry=" + RegistryHost() }

// registriesFile is where the k3s recipe's node reads its registries.yaml,
// in the folder it writes its kubeconfig to, which is this computer's.
const registriesFile = "/output/registries.yaml"

// privateRegistry is the k3s flag that names the file.
func privateRegistry() string { return "--private-registry=" + registriesFile }

// RegistriesYAML is the node's registries.yaml: both names of GVX Desktop's
// built-in local OCI registry (serving port 5001) lead to it over HTTP.
// It does nothing while the registry is not running but fail the pulls of
// its images.
func RegistriesYAML() string {
	endpoint := "http://" + RegistryHost()
	var b strings.Builder
	b.WriteString("# Written by GVX Desktop at each start of the cluster: the local image\n# registry, as the cluster's nodes reach it.\nmirrors:\n")
	for _, name := range []string{RegistryLocal(), RegistryHost()} {
		fmt.Fprintf(&b, "  %q:\n    endpoint:\n      - %q\n", name, endpoint)
	}
	return b.String()
}

// flagValue is the value of a --name=value or --name value flag in args.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true
		}
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// Upgrade brings a recipe's sandbox saved by an earlier version of the app
// to what the recipe makes now, keeping what the user changed of it: the
// engine pushes to the image registry, publishes dockerd on its app-private
// socket, and publishes its containers' ports by itself, and the cluster's
// nodes pull from the registry. It reports whether it changed anything; the
// change takes a restart.
func Upgrade(spec sandbox.Spec) (sandbox.Spec, bool) {
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return spec, false
	}
	cmdChanged := false
	switch spec.Recipe {
	case Docker:
		changed := false
		if !HasDockerConfig(spec) {
			// dockerd reads daemon.json from this computer, which names
			// the registry: the flag would clash with it.
			spec = spec.Clone()
			spec.Mounts = append(spec.Mounts, dockerConfigMount())
			if i := slices.Index(args, insecureRegistry()); i >= 0 {
				args = slices.Delete(args, i, i+1)
				cmdChanged = true
			}
			changed = true
		}
		// -auto-publish=false in the extra flags keeps it off.
		if _, err := rungv.SplitArgs(spec.ExtraArgs); err == nil && !sandbox.AutoPublishSet(spec) {
			spec = sandbox.WithAutoPublish(spec, true)
			changed = true
		}
		if ports, ok := upgradeDockerPorts(spec.Ports); ok {
			spec = spec.Clone()
			spec.Ports = ports
			changed = true
		}
		if !changed {
			return spec, false
		}
	case Kubernetes:
		if _, ok := flagValue(args, "--private-registry"); ok {
			return spec, false
		}
		if !slices.ContainsFunc(spec.Mounts, func(m sandbox.Mount) bool { return m.Type == sandbox.MountBind && m.Target == path.Dir(registriesFile) }) {
			return spec, false // the folder the file goes in is not this computer's
		}
		args = append(args, privateRegistry())
		cmdChanged = true
	default:
		return spec, false
	}
	spec = spec.Clone()
	if cmdChanged {
		spec.Command = rungv.FormatArgs(args)
	}
	return spec, true
}

// EnsureDockerBackendPort returns a copy of spec with DockerBackendPublish
// carrying guest port 2375, replacing any legacy API publish and restoring it
// if missing while keeping every other port untouched.
func EnsureDockerBackendPort(spec sandbox.Spec) sandbox.Spec {
	ports, changed := upgradeDockerPorts(spec.Ports)
	if !changed {
		return spec
	}
	spec = spec.Clone()
	spec.Ports = ports
	return spec
}

// upgradeDockerPorts rewrites an engine spec's API port entry (an old host-facing
// Unix socket or TCP publish) to the app-private backend socket, or prepends it
// when no API port entry is present, keeping every other port untouched.
func upgradeDockerPorts(ports []string) ([]string, bool) {
	out := make([]string, 0, len(ports)+1)
	placed := false
	changed := false
	for _, p := range ports {
		if isDockerAPI(p) {
			if !placed {
				out = append(out, DockerBackendPublish)
				placed = true
				if p != DockerBackendPublish {
					changed = true
				}
			} else {
				changed = true
			}
			continue
		}
		out = append(out, p)
	}
	if !placed {
		out = append([]string{DockerBackendPublish}, out...)
		changed = true
	}
	if !changed {
		return ports, false
	}
	return out, true
}

// Prepare writes the files of this computer a recipe's sandbox reads as it
// starts: the engine's daemon.json, and the cluster's registries.yaml, in the folder of this computer its
// --private-registry is in. It leaves alone a sandbox that names no such
// file.
func Prepare(spec sandbox.Spec) error {
	if spec.Recipe == Docker {
		return prepareDockerConfig(spec)
	}
	if !IsKubernetesNode(spec) {
		return nil
	}
	args, err := rungv.SplitArgs(spec.Command)
	if err != nil {
		return nil
	}
	file, ok := flagValue(args, "--private-registry")
	if !ok || file != registriesFile {
		return nil // the user's own file, or none
	}
	for _, m := range spec.Mounts {
		if m.Type != sandbox.MountBind || !under(file, m.Target) || m.ReadOnly {
			continue
		}
		host := filepath.Join(rungv.ExpandHome(m.Source), filepath.FromSlash(strings.TrimPrefix(file, m.Target)))
		want := []byte(RegistriesYAML())
		if have, err := os.ReadFile(host); err == nil && bytes.Equal(have, want) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			return err
		}
		return os.WriteFile(host, want, 0o644)
	}
	return nil
}
