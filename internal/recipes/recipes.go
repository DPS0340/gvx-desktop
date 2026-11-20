// Package recipes holds presets for the workloads gvx documents: a Docker
// Engine (docker:dind), a Kubernetes node (rancher/k3s), and an image
// registry (registry:2) between the two.
package recipes

import (
	"fmt"
	"net"
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

// DockerSocket is the host socket the app serves the Docker API on.
func DockerSocket() string { return rungv.ExpandHome("~/.gvx/docker.sock") }

// dockerBackendSocketPath is the unexpanded path of the app-private Unix
// socket the Docker Engine sandbox publishes dockerd on.
const dockerBackendSocketPath = "~/.gvx/engine/dockerd.sock"

// dockerGuestPort is where dockerd listens inside the sandbox.
const dockerGuestPort = 2375

// DockerBackendPublish is the sandbox port publish carrying dockerd's API to
// the app-private Unix socket on every OS.
const DockerBackendPublish = "unix:" + dockerBackendSocketPath + ":2375"

// DockerBackendSocket is the app-private Unix socket the Docker Engine sandbox
// publishes dockerd on. Only the app connects to it.
// Note: Unix domain socket paths are subject to OS sun_path limits (~104 bytes
// on macOS, ~108 bytes on Linux and Windows), so very long home directory paths
// may exceed that limit.
func DockerBackendSocket() string { return rungv.ExpandHome(dockerBackendSocketPath) }

// DockerBackendHost is the DOCKER_HOST value the app's own client uses to
// reach dockerd directly, bypassing the proxy.
func DockerBackendHost() string { return "unix://" + filepath.ToSlash(DockerBackendSocket()) }

// DefaultDockerPort is the host port the app's proxy takes over TCP.
const DefaultDockerPort = 2375

// DockerAPI is how docker reaches the engine from this computer through the
// app's proxy: on the Unix socket ~/.gvx/docker.sock, or over TCP on a port
// of the loopback address. A Docker CLI on Windows reaches a Unix socket only
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

// ListenAddr returns the network and address the app's proxy listens on.
func (a DockerAPI) ListenAddr() (network, address string) {
	if a.TCP {
		return "tcp", fmt.Sprintf("127.0.0.1:%d", a.port())
	}
	return "unix", DockerSocket()
}

// Host is the DOCKER_HOST value that reaches the engine.
func (a DockerAPI) Host() string {
	if a.TCP {
		return fmt.Sprintf("tcp://127.0.0.1:%d", a.port())
	}
	return "unix://" + filepath.ToSlash(DockerSocket())
}

// DockerHost is the DOCKER_HOST value that reaches an engine with the
// defaults.
func DockerHost() string { return DefaultDockerAPI().Host() }

// parseDockerAPIPort reports whether a published port carries the engine's API
// on guest port 2375, and how it was published (Unix socket or TCP with its
// host port). It accepts rungv's -p syntax: [ip:][hostPort:]guestPort[/proto]
// (ignoring /udp and port ranges) as well as unix:PATH:2375.
func parseDockerAPIPort(port string) (DockerAPI, bool) {
	p := strings.TrimSpace(port)
	if rest, ok := strings.CutPrefix(p, "unix:"); ok {
		i := strings.LastIndexByte(rest, ':')
		if i <= 0 {
			return DockerAPI{}, false
		}
		guest := rest[i+1:]
		if base, proto, hasProto := strings.Cut(guest, "/"); hasProto {
			if !strings.EqualFold(proto, "tcp") {
				return DockerAPI{}, false
			}
			guest = base
		}
		if guest != strconv.Itoa(dockerGuestPort) {
			return DockerAPI{}, false
		}
		return DockerAPI{Port: DefaultDockerPort}, true
	}
	if base, proto, hasProto := strings.Cut(p, "/"); hasProto {
		if !strings.EqualFold(proto, "tcp") {
			return DockerAPI{}, false
		}
		p = base
	}
	lastColon := strings.LastIndexByte(p, ':')
	if lastColon < 0 {
		if p == strconv.Itoa(dockerGuestPort) {
			return DockerAPI{TCP: true, Port: DefaultDockerPort}, true
		}
		return DockerAPI{}, false
	}
	before, guest := p[:lastColon], p[lastColon+1:]
	if guest != strconv.Itoa(dockerGuestPort) {
		return DockerAPI{}, false
	}
	// hostPort:guestPort
	if hostPort, err := strconv.Atoi(before); err == nil && hostPort >= 1 && hostPort <= 65535 {
		return DockerAPI{TCP: true, Port: hostPort}, true
	}
	// ip:guestPort (e.g. 127.0.0.1:2375 or [::1]:2375)
	if isBindIP(before) {
		return DockerAPI{TCP: true, Port: DefaultDockerPort}, true
	}
	// ip:hostPort:guestPort (e.g. 127.0.0.1:23750:2375 or [::1]:23750:2375)
	if i := strings.LastIndexByte(before, ':'); i > 0 {
		ipPart, hostPart := before[:i], before[i+1:]
		if isBindIP(ipPart) {
			if hostPort, err := strconv.Atoi(hostPart); err == nil && hostPort >= 1 && hostPort <= 65535 {
				return DockerAPI{TCP: true, Port: hostPort}, true
			}
		}
	}
	return DockerAPI{}, false
}

func isBindIP(s string) bool {
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	return net.ParseIP(s) != nil
}

// isDockerAPI reports whether a published port carries the engine's API.
func isDockerAPI(port string) bool {
	_, ok := parseDockerAPIPort(port)
	return ok
}

// LegacyDockerAPI returns the DockerAPI endpoint a pre-migration Docker Engine
// spec published directly (TCP or Unix), if any.
func LegacyDockerAPI(spec sandbox.Spec) (DockerAPI, bool) {
	for _, p := range spec.Ports {
		if api, ok := parseDockerAPIPort(p); ok {
			return api, true
		}
	}
	return DockerAPI{}, false
}

// DockerEngine runs an unmodified dockerd in a sandbox with its data in the
// ext4lite volume gvx-docker, its API published on the app-private Unix socket
// ~/.gvx/engine/dockerd.sock, the ports its containers publish reaching this
// computer under the same numbers (rungv's -auto-publish; 9000-9019 are
// published besides, for a rungv that has no such flag) and the user's home
// folder shared at its own path (docs/recipes/docker-engine.md).
func DockerEngine() sandbox.Spec {
	return WithSharedFolders(dockerEngine(), DefaultSharedFolders())
}

func dockerEngine() sandbox.Spec {
	return sandbox.Spec{
		ID:           sandbox.NewID(),
		Name:         "docker-engine",
		Image:        "docker:dind",
		Command:      "--tls=false " + insecureRegistry(),
		Env:          []string{"DOCKER_TLS_CERTDIR="},
		Ports:        []string{DockerBackendPublish, "127.0.0.1:9000-9019:9000-9019"},
		Mounts:       []sandbox.Mount{{Type: sandbox.MountVolume, Source: "gvx-docker", Target: "/var/lib/docker"}},
		ExtraArgs:    "-auto-publish",
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
// (docs/recipes/kubernetes.md). Its load balancer, ServiceLB, is off.
func KubernetesNode() sandbox.Spec { return KubernetesNodeWith(false) }

// KubernetesNodeWith is KubernetesNode with ServiceLB on when serviceLB is
// set, and the node's ports 80 and 443 published for it.
func KubernetesNodeWith(serviceLB bool) sandbox.Spec {
	spec := sandbox.Spec{
		ID:      sandbox.NewID(),
		Name:    "k3s",
		Image:   K3sImage,
		Command: k3sCommand(false),
		Env:     []string{"IPTABLES_MODE=legacy"},
		Ports:   []string{fmt.Sprintf("127.0.0.1:%d:%d", k3sAPIPort, k3sAPIPort), "127.0.0.1:30000-30019:30000-30019"},
		Mounts: []sandbox.Mount{
			{Type: sandbox.MountVolume, Source: "k3s", Target: "/var/lib/rancher"},
			{Type: sandbox.MountBind, Source: "~/.gvx/k3s", Target: "/output"},
		},
		Recipe:       Kubernetes,
		ReadyPattern: "k3s is up and running",
		CreatedAt:    time.Now(),
	}
	if serviceLB {
		spec = WithServiceLB(spec, true)
	}
	return spec
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
				"Published container ports reach this computer under the same number",
				"Your home folder is shared, so compose bind mounts work",
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
				"Optional load balancer on 127.0.0.1:8080 and :8443",
				"Cluster state persists in the k3s volume",
			},
			New: KubernetesNode,
		},
		{
			Key:     Registry,
			Title:   "Image registry",
			Summary: "A registry on this computer: push images from the Docker Engine, and run them in the cluster.",
			Image:   RegistryImage,
			Highlights: []string{
				"Push to host.docker.internal:5001 from the Docker Engine",
				"Pods pull localhost:5001/NAME:TAG and host.docker.internal:5001/NAME:TAG",
				"Images persist in the gvx-registry volume",
			},
			New: ImageRegistry,
		},
	}
}
