// Package ext implements the core GVX Desktop extension host:
// manifest parsing, permission grants, KV storage, secret vault,
// per-extension settings, Git/local/OCI source acquisition, and BuildKit image builds.
package ext

import (
	"context"
	"time"
)

const (
	// APIVersion is the Host/Extension protocol and manifest schema version.
	APIVersion = 1

	// ManifestName is the filename of an extension manifest.
	ManifestName = "extension.toml"

	// DefaultRuntimePort is the default container/extension HTTP port.
	DefaultRuntimePort = 18900

	// DefaultRuntimeMemory is the default sandbox memory limit.
	DefaultRuntimeMemory = "512m"
)

// ExtensionState represents the lifecycle state of an installed extension.
type ExtensionState string

const (
	StateDiscovered ExtensionState = "discovered"
	StateInvalid    ExtensionState = "invalid"
	StateWaiting    ExtensionState = "waiting"
	StateBuilding   ExtensionState = "building"
	StateDisabled   ExtensionState = "disabled"
	StateStarting   ExtensionState = "starting"
	StateRunning    ExtensionState = "running"
	StateDegraded   ExtensionState = "degraded"
	StateCrashed    ExtensionState = "crashed"
	StateFailed     ExtensionState = "failed"
)

// IsActive reports whether the extension is starting, running, or degraded.
func (s ExtensionState) IsActive() bool {
	return s == StateStarting || s == StateRunning || s == StateDegraded
}

// CanRun reports whether the extension is eligible to be started.
func (s ExtensionState) CanRun() bool {
	return s != StateInvalid && s != StateWaiting && s != StateBuilding && s != StateDisabled && s != StateFailed
}

// SourceKind identifies how an extension was installed.
const (
	SourceGit   = "git"
	SourceLocal = "local"
	SourceImage = "image"
)

// SourceSpec persists how an extension was acquired and its built/pulled image metadata.
type SourceSpec struct {
	Kind      string `json:"kind"`
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	ImageRef  string `json:"image_ref,omitempty"`
	ImageID   string `json:"image_id,omitempty"`
	BuiltAt   string `json:"built_at,omitempty"`
}

// ViewInfo describes a contributed extension view for UI navigation.
type ViewInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Icon   string `json:"icon,omitempty"`
	Chrome string `json:"chrome"`
}

// CommandInfo describes a contributed command palette entry.
type CommandInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ToolParamInfo describes a parameter of a contributed MCP tool.
type ToolParamInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ToolInfo describes a contributed MCP tool.
type ToolInfo struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Params      []ToolParamInfo `json:"params,omitempty"`
}

// SettingSummary describes a setting for UI rendering; secret values are never populated.
type SettingSummary struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Options     []string `json:"options,omitempty"`
	Default     any      `json:"default,omitempty"`
	Set         bool     `json:"set"`
	Value       string   `json:"value,omitempty"`
}

// WebUIInfo describes an extension's optional localproxy web UI endpoint.
type WebUIInfo struct {
	Port  int    `json:"port"`
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// BuildLogEntry is one timestamped build output line.
type BuildLogEntry struct {
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

// SecretStoreStatus describes where secrets are currently persisted.
type SecretStoreStatus struct {
	Store  string `json:"store"`
	Detail string `json:"detail,omitempty"`
}

// BadgeInfo describes a sidebar badge pushed via POST /v1/ui/badge.
type BadgeInfo struct {
	Text    string `json:"text,omitempty"`
	Tone    string `json:"tone,omitempty"`
	Tooltip string `json:"tooltip,omitempty"`
}

// ExtensionInfo is the host's read-only view of an extension for UI and status reporting.
type ExtensionInfo struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	Description    string           `json:"description,omitempty"`
	Icon           string           `json:"icon,omitempty"`
	Version        string           `json:"version"`
	Publisher      string           `json:"publisher"`
	Bundled        bool             `json:"bundled,omitempty"`
	State          ExtensionState   `json:"state"`
	Error          string           `json:"error,omitempty"`
	Badge          BadgeInfo        `json:"badge,omitempty"`
	Source         SourceSpec       `json:"source"`
	RuntimeType    string           `json:"runtime_type"`
	Unsandboxed    bool             `json:"unsandboxed,omitempty"`
	Permissions    []string         `json:"permissions,omitempty"`
	Granted        []string         `json:"granted,omitempty"`
	Denied         []string         `json:"denied,omitempty"`
	Asked          []string         `json:"asked,omitempty"`
	AllowedFolders []string         `json:"allowed_folders,omitempty"`
	Views          []ViewInfo       `json:"views,omitempty"`
	Commands       []CommandInfo    `json:"commands,omitempty"`
	Tools          []ToolInfo       `json:"tools,omitempty"`
	Settings       []SettingSummary `json:"settings,omitempty"`
	WebUI          *WebUIInfo       `json:"web_ui,omitempty"`
	StorageUsed    int              `json:"storage_used"`
	StorageQuota   int              `json:"storage_quota"`
}

// BuildRequest configures a BuildKit OCI image build for an extension.
type BuildRequest struct {
	Dockerfile string
	ContextDir string
	Target     string
	Platform   string
	Tags       []string
	Labels     map[string]string
	BuildArgs  map[string]string
	NoCache    bool
	Pull       bool
}

// BuildResult holds the resulting OCI image ID and tags from a build.
type BuildResult struct {
	ImageID string
	Tags    []string
}

// ImageBuilder builds an OCI image from a local context directory and streams build logs.
type ImageBuilder interface {
	Build(ctx context.Context, req BuildRequest, onLine func(string)) (imageID string, err error)
}

// ImagePuller pulls a prebuilt OCI image reference (implemented by *rungv.Client).
type ImagePuller interface {
	Pull(ctx context.Context, ref string, line func(string)) error
}
