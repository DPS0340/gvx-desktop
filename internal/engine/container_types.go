package engine

// Owner: workstream B. Add fields freely; the fields marked "relied on"
// are read by the other workstreams: do not rename or remove them, nor
// change what they mean.

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Container is the engine's record of one container, as containers.json
// keeps it.
type Container struct {
	ID      string    `json:"id"`   // relied on: 64 hex digits, also the sandbox's Spec.ID
	Name    string    `json:"name"` // relied on: without the leading slash
	Created time.Time `json:"created"`
	Image   string    `json:"image"`   // relied on: the reference the client gave
	ImageID string    `json:"imageId"` // relied on: sha256:<64 hex>

	// Config is the client's config merged with the image's: what the
	// container runs, and what inspect shows.
	Config     ContainerConfig `json:"config"`     // relied on: Tty, OpenStdin, StdinOnce, Labels, Image
	HostConfig HostConfig      `json:"hostConfig"` // relied on: AutoRemove, NetworkMode
	// Networks are the networks the container joined, by name.
	Networks map[string]EndpointSettings `json:"networks,omitempty"` // relied on: the keys
	// Mounts are the mounts the sandbox gets, resolved at create.
	Mounts []MountPoint `json:"mounts,omitempty"` // relied on: Type, Name, Source, Destination
	// AnonymousVolumes lists the anonymous volume names created for this
	// container, persisted so rm -v and AutoRemove work after a restart.
	AnonymousVolumes []string `json:"anonymousVolumes,omitempty"`
	// Spec is the sandbox the container runs; its ports are set at each
	// start.
	Spec sandbox.Spec `json:"spec"`
	// Ports are the published ports of the current or last run, by
	// container port ("80/tcp").
	Ports map[string][]PortBinding `json:"ports,omitempty"`

	State        ContainerState `json:"state"` // relied on
	RestartCount int            `json:"restartCount"`

	// Runtime-only bookkeeping guarded by containerState.mu.
	runGen          uint64        `json:"-"`
	startEmittedGen uint64        `json:"-"`
	restartGen      uint64        `json:"-"`
	backoff         time.Duration `json:"-"`
	restartTimer    *time.Timer   `json:"-"`
}

// clone returns a deep copy of c safe to inspect without holding locks.
func (c *Container) clone() Container {
	cp := *c
	cp.restartTimer = nil
	cp.Config = c.Config.clone()
	cp.HostConfig = c.HostConfig.clone()
	if c.Networks != nil {
		cp.Networks = make(map[string]EndpointSettings, len(c.Networks))
		for k, v := range c.Networks {
			v.Aliases = slices.Clone(v.Aliases)
			v.DNSNames = slices.Clone(v.DNSNames)
			cp.Networks[k] = v
		}
	}
	cp.Mounts = slices.Clone(c.Mounts)
	cp.AnonymousVolumes = slices.Clone(c.AnonymousVolumes)
	cp.Spec = c.Spec.Clone()
	if c.Ports != nil {
		cp.Ports = make(map[string][]PortBinding, len(c.Ports))
		for k, v := range c.Ports {
			cp.Ports[k] = slices.Clone(v)
		}
	}
	return cp
}

// The container statuses, as docker names them.
const (
	StatusCreated    = "created"
	StatusRunning    = "running"
	StatusRestarting = "restarting"
	StatusRemoving   = "removing"
	StatusExited     = "exited"
	StatusDead       = "dead"
)

// ContainerState is where a container is in its life.
type ContainerState struct {
	Status     string    `json:"status"` // relied on: one of the Status constants
	ExitCode   int       `json:"exitCode"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	OOMKilled  bool      `json:"oomKilled,omitempty"`
	Pid        int       `json:"-"`
	// StoppedByUser is set by stop and kill, and cleared by start: the
	// restart policies leave such a container stopped.
	StoppedByUser bool `json:"stoppedByUser,omitempty"`
	// Exits counts the runs that ended, for wait?condition=next-exit.
	Exits int `json:"exits"` // relied on
}

// Running reports whether the container is active (running or restarting).
func (s ContainerState) Running() bool {
	return s.Status == StatusRunning || s.Status == StatusRestarting
}

// ContainerConfig is docker's container Config, the part the engine keeps.
type ContainerConfig struct {
	Hostname     string              `json:"Hostname"`
	Domainname   string              `json:"Domainname"`
	User         string              `json:"User"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Tty          bool                `json:"Tty"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Healthcheck  json.RawMessage     `json:"Healthcheck,omitempty"`
	Image        string              `json:"Image"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	WorkingDir   string              `json:"WorkingDir"`
	Entrypoint   []string            `json:"Entrypoint"`
	Labels       map[string]string   `json:"Labels"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
}

func (c ContainerConfig) clone() ContainerConfig {
	cp := c
	cp.ExposedPorts = maps.Clone(c.ExposedPorts)
	cp.Env = slices.Clone(c.Env)
	cp.Cmd = slices.Clone(c.Cmd)
	cp.Healthcheck = slices.Clone(c.Healthcheck)
	cp.Volumes = maps.Clone(c.Volumes)
	cp.Entrypoint = slices.Clone(c.Entrypoint)
	cp.Labels = maps.Clone(c.Labels)
	if c.StopTimeout != nil {
		v := *c.StopTimeout
		cp.StopTimeout = &v
	}
	return cp
}

// HostConfig is docker's HostConfig, the part the engine keeps.
type HostConfig struct {
	Binds           []string                 `json:"Binds"`
	NetworkMode     string                   `json:"NetworkMode"`
	PortBindings    map[string][]PortBinding `json:"PortBindings"`
	RestartPolicy   RestartPolicy            `json:"RestartPolicy"`
	AutoRemove      bool                     `json:"AutoRemove"`
	Mounts          []MountSpec              `json:"Mounts,omitempty"`
	ConsoleSize     [2]uint                  `json:"ConsoleSize"`
	ExtraHosts      []string                 `json:"ExtraHosts"`
	Privileged      bool                     `json:"Privileged"`
	PublishAllPorts bool                     `json:"PublishAllPorts"`
	ReadonlyRootfs  bool                     `json:"ReadonlyRootfs"`
	Tmpfs           map[string]string        `json:"Tmpfs,omitempty"`
	Memory          int64                    `json:"Memory"`
	NanoCpus        int64                    `json:"NanoCpus"`
}

func (h HostConfig) clone() HostConfig {
	cp := h
	cp.Binds = slices.Clone(h.Binds)
	if h.PortBindings != nil {
		cp.PortBindings = make(map[string][]PortBinding, len(h.PortBindings))
		for k, v := range h.PortBindings {
			cp.PortBindings[k] = slices.Clone(v)
		}
	}
	if h.Mounts != nil {
		cp.Mounts = make([]MountSpec, len(h.Mounts))
		for i, m := range h.Mounts {
			if m.TmpfsOptions != nil {
				opts := *m.TmpfsOptions
				m.TmpfsOptions = &opts
			}
			cp.Mounts[i] = m
		}
	}
	cp.ExtraHosts = slices.Clone(h.ExtraHosts)
	cp.Tmpfs = maps.Clone(h.Tmpfs)
	return cp
}

// PortBinding is a host address a container port is published on.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy is no, always, unless-stopped or on-failure.
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// MountSpec is one entry of HostConfig.Mounts.
type MountSpec struct {
	Type         string        `json:"Type"` // bind, volume, tmpfs
	Source       string        `json:"Source,omitempty"`
	Target       string        `json:"Target"`
	ReadOnly     bool          `json:"ReadOnly,omitempty"`
	TmpfsOptions *TmpfsOptions `json:"TmpfsOptions,omitempty"`
}

// TmpfsOptions are the options of a tmpfs mount.
type TmpfsOptions struct {
	SizeBytes int64 `json:"SizeBytes,omitempty"`
	Mode      int   `json:"Mode,omitempty"`
}

// MountPoint is a mount as inspect shows it.
type MountPoint struct {
	Type        string `json:"Type"`           // bind, volume, tmpfs
	Name        string `json:"Name,omitempty"` // the volume's
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver,omitempty"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
	// Anonymous marks a volume made for the container, which rm -v and
	// AutoRemove delete.
	Anonymous bool `json:"-"`
}

// EndpointSettings is a container's place on a network.
type EndpointSettings struct {
	Aliases     []string `json:"Aliases"`
	NetworkID   string   `json:"NetworkID"`
	EndpointID  string   `json:"EndpointID"`
	Gateway     string   `json:"Gateway"`
	IPAddress   string   `json:"IPAddress"`
	IPPrefixLen int      `json:"IPPrefixLen"`
	MacAddress  string   `json:"MacAddress"`
	DNSNames    []string `json:"DNSNames"`
}

// strSlice decodes a Docker string-or-slice field while distinguishing
// absent/null (nil) from an explicit empty array ([]string{}).
type strSlice []string

func (s *strSlice) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if bytes.Equal(trimmed, []byte("null")) {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(trimmed, &one); err == nil {
		*s = strSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return err
	}
	if many == nil {
		many = []string{}
	}
	*s = strSlice(many)
	return nil
}

// containerCreateRequest is the body of POST /containers/create.
type containerCreateRequest struct {
	Hostname         string              `json:"Hostname"`
	Domainname       string              `json:"Domainname"`
	User             string              `json:"User"`
	AttachStdin      bool                `json:"AttachStdin"`
	AttachStdout     bool                `json:"AttachStdout"`
	AttachStderr     bool                `json:"AttachStderr"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts"`
	Tty              bool                `json:"Tty"`
	OpenStdin        bool                `json:"OpenStdin"`
	StdinOnce        bool                `json:"StdinOnce"`
	Env              []string            `json:"Env"`
	Cmd              strSlice            `json:"Cmd"`
	Healthcheck      json.RawMessage     `json:"Healthcheck"`
	Image            string              `json:"Image"`
	Volumes          map[string]struct{} `json:"Volumes"`
	WorkingDir       string              `json:"WorkingDir"`
	Entrypoint       strSlice            `json:"Entrypoint"`
	Labels           map[string]string   `json:"Labels"`
	StopSignal       string              `json:"StopSignal"`
	StopTimeout      *int                `json:"StopTimeout"`
	HostConfig       HostConfig          `json:"HostConfig"`
	NetworkingConfig *networkingConfig   `json:"NetworkingConfig"`
}

type networkingConfig struct {
	EndpointsConfig map[string]*endpointConfig `json:"EndpointsConfig"`
}

type endpointConfig struct {
	Aliases    []string `json:"Aliases"`
	IPAMConfig *struct {
		IPv4Address string `json:"IPv4Address"`
	} `json:"IPAMConfig"`
	IPAddress string `json:"IPAddress"`
}

// ipv4 is the address the client asked for, or "".
func (c *endpointConfig) ipv4() string {
	if c == nil {
		return ""
	}
	if c.IPAMConfig != nil && c.IPAMConfig.IPv4Address != "" {
		return c.IPAMConfig.IPv4Address
	}
	return c.IPAddress
}

type containerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type containerListItem struct {
	ID              string              `json:"Id"`
	Names           []string            `json:"Names"`
	Image           string              `json:"Image"`
	ImageID         string              `json:"ImageID"`
	Command         string              `json:"Command"`
	Created         int64               `json:"Created"`
	Ports           []containerListPort `json:"Ports"`
	Labels          map[string]string   `json:"Labels"`
	State           string              `json:"State"`
	Status          string              `json:"Status"`
	HostConfig      listHostConfig      `json:"HostConfig"`
	NetworkSettings listNetworkSettings `json:"NetworkSettings"`
	Mounts          []MountPoint        `json:"Mounts"`
}

type containerListPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

type listHostConfig struct {
	NetworkMode string `json:"NetworkMode"`
}

type listNetworkSettings struct {
	Networks map[string]EndpointSettings `json:"Networks"`
}

type containerInspectResponse struct {
	ID              string                 `json:"Id"`
	Created         string                 `json:"Created"`
	Path            string                 `json:"Path"`
	Args            []string               `json:"Args"`
	State           inspectState           `json:"State"`
	Image           string                 `json:"Image"`
	ResolvConfPath  string                 `json:"ResolvConfPath"`
	HostnamePath    string                 `json:"HostnamePath"`
	HostsPath       string                 `json:"HostsPath"`
	LogPath         string                 `json:"LogPath"`
	Name            string                 `json:"Name"`
	RestartCount    int                    `json:"RestartCount"`
	Driver          string                 `json:"Driver"`
	Platform        string                 `json:"Platform"`
	MountLabel      string                 `json:"MountLabel"`
	ProcessLabel    string                 `json:"ProcessLabel"`
	AppArmorProfile string                 `json:"AppArmorProfile"`
	ExecIDs         []string               `json:"ExecIDs"`
	HostConfig      HostConfig             `json:"HostConfig"`
	GraphDriver     inspectGraphDriver     `json:"GraphDriver"`
	Mounts          []MountPoint           `json:"Mounts"`
	Config          ContainerConfig        `json:"Config"`
	NetworkSettings inspectNetworkSettings `json:"NetworkSettings"`
}

type inspectState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

type inspectGraphDriver struct {
	Name string            `json:"Name"`
	Data map[string]string `json:"Data"`
}

type inspectNetworkSettings struct {
	Bridge                 string                      `json:"Bridge"`
	SandboxID              string                      `json:"SandboxID"`
	SandboxKey             string                      `json:"SandboxKey"`
	Ports                  map[string][]PortBinding    `json:"Ports"`
	HairpinMode            bool                        `json:"HairpinMode"`
	LinkLocalIPv6Address   string                      `json:"LinkLocalIPv6Address"`
	LinkLocalIPv6PrefixLen int                         `json:"LinkLocalIPv6PrefixLen"`
	Gateway                string                      `json:"Gateway"`
	IPAddress              string                      `json:"IPAddress"`
	IPPrefixLen            int                         `json:"IPPrefixLen"`
	MacAddress             string                      `json:"MacAddress"`
	Networks               map[string]EndpointSettings `json:"Networks"`
}

type containerPruneResponse struct {
	ContainersDeleted []string `json:"ContainersDeleted"`
	SpaceReclaimed    uint64   `json:"SpaceReclaimed"`
}

type containerStoreFile struct {
	Version    int         `json:"version"`
	Containers []Container `json:"containers"`
}
