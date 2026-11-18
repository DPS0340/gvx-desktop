// Package recipes holds presets for the workloads gvx documents: a Docker
// Engine (docker:dind) and a Kubernetes node (rancher/k3s).
package recipes

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const (
	Docker     = "docker"
	Kubernetes = "k3s"
)

// DockerSocket is the host socket the Docker Engine recipe publishes.
func DockerSocket() string { return rungv.ExpandHome("~/.gvx/docker.sock") }

// dockerGuestPort is where dockerd listens inside the sandbox.
const dockerGuestPort = 2375

// DefaultDockerPort is the host port the engine's API takes over TCP.
const DefaultDockerPort = 2375

// DockerAPI is how the recipe's engine is reached from the host: through
// the Unix socket ~/.gvx/docker.sock, or over TCP on a port of the
// loopback address. A Docker CLI on Windows reaches a Unix socket only
// from version 29 on; TCP works with any, on every system.
type DockerAPI struct {
	TCP  bool
	Port int // the host port, for TCP
}

// DefaultDockerAPI is the socket where sockets serve every CLI, and TCP on
// Windows.
func DefaultDockerAPI() DockerAPI {
	return DockerAPI{TCP: runtime.GOOS == "windows", Port: DefaultDockerPort}
}

func (a DockerAPI) port() int {
	if a.Port <= 0 || a.Port > 65535 {
		return DefaultDockerPort
	}
	return a.Port
}

// Publish is the sandbox's published port that carries the API.
func (a DockerAPI) Publish() string {
	if a.TCP {
		return fmt.Sprintf("127.0.0.1:%d:%d", a.port(), dockerGuestPort)
	}
	return fmt.Sprintf("unix:~/.gvx/docker.sock:%d", dockerGuestPort)
}

// Host is the DOCKER_HOST value that reaches the engine.
func (a DockerAPI) Host() string {
	if a.TCP {
		return fmt.Sprintf("tcp://127.0.0.1:%d", a.port())
	}
	return "unix://" + filepath.ToSlash(DockerSocket())
}

// DockerHost is the DOCKER_HOST value that reaches an engine made with the
// defaults.
func DockerHost() string { return DefaultDockerAPI().Host() }

// isDockerAPI reports whether a published port carries the engine's API,
// and how.
func isDockerAPI(port string) (DockerAPI, bool) {
	parts := strings.Split(port, ":")
	if len(parts) < 2 || parts[len(parts)-1] != strconv.Itoa(dockerGuestPort) {
		return DockerAPI{}, false
	}
	if parts[0] == "unix" {
		return DockerAPI{Port: DefaultDockerPort}, true
	}
	host, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return DockerAPI{}, false
	}
	return DockerAPI{TCP: true, Port: host}, true
}

// DockerAPIOf reads from an engine's sandbox how its API is published,
// which is what counts once the sandbox exists: its ports are its own.
func DockerAPIOf(spec sandbox.Spec) (DockerAPI, bool) {
	for _, p := range spec.Ports {
		if api, ok := isDockerAPI(p); ok {
			return api, true
		}
	}
	return DockerAPI{}, false
}

// WithDockerAPI returns the engine's sandbox publishing its API as api
// says, its other ports untouched. The change takes a restart: a sandbox's
// ports are fixed when it starts.
func WithDockerAPI(spec sandbox.Spec, api DockerAPI) sandbox.Spec {
	ports := make([]string, 0, len(spec.Ports)+1)
	placed := false
	for _, p := range spec.Ports {
		if _, ok := isDockerAPI(p); ok {
			if !placed {
				ports = append(ports, api.Publish())
				placed = true
			}
			continue
		}
		ports = append(ports, p)
	}
	if !placed {
		ports = append([]string{api.Publish()}, ports...)
	}
	spec.Ports = ports
	return spec
}

// DockerEngine runs an unmodified dockerd in a sandbox with its data in the
// ext4lite volume gvx-docker, its API on ~/.gvx/docker.sock (TCP on
// Windows) and the host ports 9000-9019 published for the containers it
// runs (docs/recipes/docker-engine.md).
func DockerEngine() sandbox.Spec { return DockerEngineWith(DefaultDockerAPI()) }

// DockerEngineWith is DockerEngine with its API published as api says.
func DockerEngineWith(api DockerAPI) sandbox.Spec {
	return sandbox.Spec{
		ID:           sandbox.NewID(),
		Name:         "docker-engine",
		Image:        "docker:dind",
		Command:      "--tls=false",
		Env:          []string{"DOCKER_TLS_CERTDIR="},
		Ports:        []string{api.Publish(), "127.0.0.1:9000-9019:9000-9019"},
		Mounts:       []sandbox.Mount{{Type: sandbox.MountVolume, Source: "gvx-docker", Target: "/var/lib/docker"}},
		Recipe:       Docker,
		ReadyPattern: "API listen on",
		CreatedAt:    time.Now(),
	}
}

// KubeconfigDir is the host directory the k3s recipe writes its kubeconfig to.
func KubeconfigDir() string { return rungv.ExpandHome("~/.gvx/k3s") }

// Kubeconfig is the kubeconfig file the k3s recipe writes.
func Kubeconfig() string { return filepath.Join(KubeconfigDir(), "kubeconfig.yaml") }

// K3sImage is the k3s release the recipe runs.
const K3sImage = "rancher/k3s:v1.31.4-k3s1"

// KubernetesNode runs a single-node k3s server with its API on 6443, the
// NodePorts 30000-30019 published and its state in the volume k3s
// (docs/recipes/kubernetes.md).
func KubernetesNode() sandbox.Spec {
	return sandbox.Spec{
		ID:      sandbox.NewID(),
		Name:    "k3s",
		Image:   K3sImage,
		Command: "server --disable=traefik,servicelb --write-kubeconfig=/output/kubeconfig.yaml --write-kubeconfig-mode=644",
		Env:     []string{"IPTABLES_MODE=legacy"},
		Ports:   []string{"127.0.0.1:6443:6443", "127.0.0.1:30000-30019:30000-30019"},
		Mounts: []sandbox.Mount{
			{Type: sandbox.MountVolume, Source: "k3s", Target: "/var/lib/rancher"},
			{Type: sandbox.MountBind, Source: "~/.gvx/k3s", Target: "/output"},
		},
		Recipe:       Kubernetes,
		ReadyPattern: "k3s is up and running",
		CreatedAt:    time.Now(),
	}
}

// Recipe describes a preset for the UI.
type Recipe struct {
	Key        string
	Title      string
	Summary    string
	Image      string
	Highlights []string
	New        func() sandbox.Spec
}

// All lists the presets.
func All() []Recipe {
	return []Recipe{
		{
			Key:     Docker,
			Title:   "Docker Engine",
			Summary: "Run dockerd without a VM or root, and drive it with the docker CLI on this machine.",
			Image:   "docker:dind",
			Highlights: []string{
				"Images and containers persist in the gvx-docker volume",
				"API on ~/.gvx/docker.sock (docker context \"gvx\")",
				"Host ports 9000–9019 reach published containers",
			},
			New: DockerEngine,
		},
		{
			Key:     Kubernetes,
			Title:   "Kubernetes (k3s)",
			Summary: "A single-node k3s cluster with a kubeconfig written to ~/.gvx/k3s.",
			Image:   K3sImage,
			Highlights: []string{
				"API server on 127.0.0.1:6443",
				"NodePorts 30000–30019 reachable from the host",
				"Cluster state persists in the k3s volume",
			},
			New: KubernetesNode,
		},
	}
}
