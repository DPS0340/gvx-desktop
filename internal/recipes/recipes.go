// Package recipes holds presets for the workloads gvx documents: a Docker
// Engine (docker:dind) and a Kubernetes node (rancher/k3s).
package recipes

import (
	"path/filepath"
	"runtime"
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

// DockerHost is the DOCKER_HOST value that reaches the recipe's engine.
func DockerHost() string {
	if runtime.GOOS == "windows" {
		return "tcp://127.0.0.1:2375"
	}
	return "unix://" + filepath.ToSlash(DockerSocket())
}

// DockerEngine runs an unmodified dockerd in a sandbox with its data in the
// ext4lite volume gvx-docker, its API on ~/.gvx/docker.sock and the host
// ports 9000-9019 published for the containers it runs
// (docs/recipes/docker-engine.md).
func DockerEngine() sandbox.Spec {
	api := "unix:~/.gvx/docker.sock:2375"
	if runtime.GOOS == "windows" {
		// A unix:// host needs Docker CLI 29 on Windows; TCP works with any.
		api = "127.0.0.1:2375:2375"
	}
	return sandbox.Spec{
		ID:           sandbox.NewID(),
		Name:         "docker-engine",
		Image:        "docker:dind",
		Command:      "--tls=false",
		Env:          []string{"DOCKER_TLS_CERTDIR="},
		Ports:        []string{api, "127.0.0.1:9000-9019:9000-9019"},
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
