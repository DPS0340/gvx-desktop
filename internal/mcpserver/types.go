// Package mcpserver implements the built-in loopback-only AI Agent MCP
// (Model Context Protocol) server for GVX Desktop over Streamable HTTP.
package mcpserver

import (
	"context"
	"time"
)

const (
	// DefaultPort is the default loopback TCP port for the built-in MCP server.
	DefaultPort = 18787

	// MaxOutputBytes caps exec stdout/stderr and log payloads returned by MCP tools.
	MaxOutputBytes = 64 * 1024

	// DefaultLogTail is the default number of trailing log lines returned by log tools.
	DefaultLogTail = 100

	// MaxLogTail is the maximum number of trailing log lines allowed by log tools.
	MaxLogTail = 2000

	truncatedMarker = "\n... [truncated]"
)

// Config configures the built-in MCP Server.
type Config struct {
	// Port selects the loopback TCP port to listen on.
	// 0 selects DefaultPort (18787); any negative value selects an ephemeral
	// 127.0.0.1:0 port (useful for tests).
	Port int

	// Version is reported in MCP Implementation.Version and /healthz (default "dev").
	Version string

	// Backend provides GVX sandbox, snapshot, Docker container, image, and route
	// operations invoked by MCP tools.
	Backend Backend

	// OnActivity is an optional callback invoked outside all internal locks after
	// each MCP tool call completes, allowing the desktop UI to update live stats.
	OnActivity func(Stats)
}

// Stats tracks live MCP tool call activity for the server.
type Stats struct {
	CallCount    int64
	LastTool     string
	LastCalledAt time.Time
	LastErr      string
}

// Equal reports whether s and o represent identical activity statistics.
func (s Stats) Equal(o Stats) bool {
	return s.CallCount == o.CallCount &&
		s.LastTool == o.LastTool &&
		s.LastCalledAt.Equal(o.LastCalledAt) &&
		s.LastErr == o.LastErr
}

// ToolSummary describes one MCP tool exposed by the server for UI display.
type ToolSummary struct {
	Name        string
	Description string
	ReadOnly    bool
	Destructive bool
}

// SandboxInfo describes a GVX sandbox returned by the backend and MCP tools.
type SandboxInfo struct {
	ID         string   `json:"id" jsonschema:"Unique sandbox identifier."`
	Name       string   `json:"name" jsonschema:"Human-readable sandbox name."`
	Image      string   `json:"image" jsonschema:"Container image used by the sandbox."`
	Status     string   `json:"status" jsonschema:"Current lifecycle status (e.g. running, stopped, paused)."`
	Paused     bool     `json:"paused" jsonschema:"Whether the sandbox is currently paused."`
	Checkpoint bool     `json:"checkpoint" jsonschema:"Whether gVisor checkpoint/restore snapshots are enabled for this sandbox."`
	Ports      []string `json:"ports,omitempty" jsonschema:"Exposed or published port mappings."`
	Recipe     string   `json:"recipe,omitempty" jsonschema:"Optional recipe name used to create the sandbox."`
	LocalURL   string   `json:"local_url,omitempty" jsonschema:"Local HTTPS proxy URL (*.localhost) if a route is active."`
}

// CreateSandboxRequest defines parameters for creating a GVX sandbox.
type CreateSandboxRequest struct {
	Name       string   `json:"name,omitempty" jsonschema:"Optional sandbox name (auto-generated if omitted)."`
	Image      string   `json:"image" jsonschema:"Container image reference to run (e.g. 'python:3.12-slim', 'node:22-alpine', 'ubuntu:24.04')."`
	Command    string   `json:"command,omitempty" jsonschema:"Optional shell command override to run inside the sandbox."`
	Env        []string `json:"env,omitempty" jsonschema:"Optional environment variables in KEY=VALUE format."`
	Ports      []string `json:"ports,omitempty" jsonschema:"Optional port mappings (e.g. '8080:80' or '3000')."`
	Workspace  string   `json:"workspace,omitempty" jsonschema:"Optional host workspace directory path to mount into the sandbox."`
	Memory     string   `json:"memory,omitempty" jsonschema:"Optional memory limit (e.g. '512m', '2g')."`
	CPUs       string   `json:"cpus,omitempty" jsonschema:"Optional CPU limit (e.g. '1', '2')."`
	Checkpoint *bool    `json:"checkpoint,omitempty" jsonschema:"Enable gVisor checkpoint/restore snapshot capability (default: true)."`
	Start      *bool    `json:"start,omitempty" jsonschema:"Start the sandbox immediately after creation (default: true)."`
}

// CheckpointEnabled returns whether checkpointing should be enabled (default true when nil).
func (r CreateSandboxRequest) CheckpointEnabled() bool {
	if r.Checkpoint == nil {
		return true
	}
	return *r.Checkpoint
}

// ShouldStart returns whether the sandbox should start immediately (default true when nil).
func (r CreateSandboxRequest) ShouldStart() bool {
	if r.Start == nil {
		return true
	}
	return *r.Start
}

// ExecResult contains the exit code and captured output of a sandbox exec call.
type ExecResult struct {
	ExitCode int    `json:"exit_code" jsonschema:"Process exit code (0 indicates success)."`
	Stdout   string `json:"stdout" jsonschema:"Captured standard output (truncated to 64 KiB if larger)."`
	Stderr   string `json:"stderr" jsonschema:"Captured standard error (truncated to 64 KiB if larger)."`
}

// SnapshotInfo describes a gVisor checkpoint snapshot of a sandbox.
type SnapshotInfo struct {
	SandboxID   string    `json:"sandbox_id" jsonschema:"ID of the sandbox this snapshot belongs to."`
	SandboxName string    `json:"sandbox_name,omitempty" jsonschema:"Name of the sandbox this snapshot belongs to."`
	Name        string    `json:"name" jsonschema:"Snapshot identifier/name."`
	Dir         string    `json:"dir,omitempty" jsonschema:"On-disk directory storing the checkpoint state."`
	Size        int64     `json:"size" jsonschema:"Total snapshot size in bytes."`
	CreatedAt   time.Time `json:"created_at" jsonschema:"Timestamp when the snapshot was created."`
}

// ContainerInfo describes a Docker container managed inside GVX Desktop.
type ContainerInfo struct {
	ID       string   `json:"id" jsonschema:"Full container ID."`
	ShortID  string   `json:"short_id" jsonschema:"Short 12-character container ID."`
	Name     string   `json:"name" jsonschema:"Container name."`
	Image    string   `json:"image" jsonschema:"Container image reference."`
	State    string   `json:"state" jsonschema:"Container state (e.g. running, exited, created)."`
	Status   string   `json:"status" jsonschema:"Human-readable status string (e.g. 'Up 5 minutes')."`
	Ports    []string `json:"ports,omitempty" jsonschema:"Published port mappings."`
	Project  string   `json:"project,omitempty" jsonschema:"Docker Compose project name, if applicable."`
	Service  string   `json:"service,omitempty" jsonschema:"Docker Compose service name, if applicable."`
	LocalURL string   `json:"local_url,omitempty" jsonschema:"Local HTTPS proxy URL (*.localhost) if a route is active."`
}

// ImageInfo describes an image from the local cache or embedded OCI registry.
type ImageInfo struct {
	Source     string   `json:"source" jsonschema:"Image source: 'cached' or 'registry'."`
	Repository string   `json:"repository" jsonschema:"Image repository name."`
	Tag        string   `json:"tag" jsonschema:"Image tag."`
	ID         string   `json:"id,omitempty" jsonschema:"Image ID (for cached images)."`
	Digest     string   `json:"digest,omitempty" jsonschema:"Content digest (e.g. sha256:...)."`
	Size       int64    `json:"size" jsonschema:"Image size in bytes."`
	SizeText   string   `json:"size_text,omitempty" jsonschema:"Human-readable size string (e.g. '42.1 MB')."`
	Platforms  []string `json:"platforms,omitempty" jsonschema:"Supported os/arch platforms."`
}

// RouteInfo describes an active *.localhost reverse proxy route.
type RouteInfo struct {
	Host     string `json:"host" jsonschema:"Local hostname (e.g. 'app.localhost')."`
	Target   string `json:"target" jsonschema:"Backend address forwarded to (e.g. '127.0.0.1:8080')."`
	Kind     string `json:"kind" jsonschema:"Target resource kind (e.g. 'sandbox', 'container', 'custom')."`
	Name     string `json:"name" jsonschema:"Associated sandbox, container, or route name."`
	HTTPURL  string `json:"http_url" jsonschema:"Local HTTP URL."`
	HTTPSURL string `json:"https_url" jsonschema:"Local HTTPS URL."`
}

// Backend decouples the MCP server from GVX Desktop's UI and VM/container subsystems.
// Methods accepting target resolve either an ID or a human-readable name.
type Backend interface {
	ListSandboxes(ctx context.Context) ([]SandboxInfo, error)
	CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxInfo, error)
	StartSandbox(ctx context.Context, target string) (SandboxInfo, error)
	StopSandbox(ctx context.Context, target string) (SandboxInfo, error)
	DeleteSandbox(ctx context.Context, target string) error
	ExecInSandbox(ctx context.Context, target string, command []string, workdir string, env []string) (ExecResult, error)
	SandboxLogs(ctx context.Context, target string, tail int) (string, error)
	CreateSnapshot(ctx context.Context, target string, leaveRunning bool) (SnapshotInfo, error)
	ListSnapshots(ctx context.Context, target string) ([]SnapshotInfo, error)
	RestoreSnapshot(ctx context.Context, target string, snapshot string) (SandboxInfo, error)
	ListContainers(ctx context.Context, all bool) ([]ContainerInfo, error)
	ContainerLogs(ctx context.Context, target string, tail int) (string, error)
	ListImages(ctx context.Context, source string) ([]ImageInfo, error)
	ListRoutes(ctx context.Context) ([]RouteInfo, error)
}
