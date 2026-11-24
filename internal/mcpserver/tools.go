package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errNoBackend = errors.New("mcpserver: backend is not configured")

var toolCatalog = []ToolSummary{
	{
		Name:        "list_sandboxes",
		Description: "List GVX sandboxes with their lifecycle status, published ports, checkpoint capability, and local HTTPS URL.",
		ReadOnly:    true,
	},
	{
		Name:        "create_sandbox",
		Description: "Create (and by default start) a GVX gVisor sandbox with checkpointing enabled so you can immediately execute commands and take snapshots.",
	},
	{
		Name:        "start_sandbox",
		Description: "Start a stopped GVX sandbox by ID or name.",
	},
	{
		Name:        "stop_sandbox",
		Description: "Stop a running GVX sandbox by ID or name.",
	},
	{
		Name:        "delete_sandbox",
		Description: "Stop and permanently delete a GVX sandbox by ID or name.",
		Destructive: true,
	},
	{
		Name:        "exec_in_sandbox",
		Description: "Execute a command inside a running GVX sandbox and return its exit code, stdout, and stderr (truncated to 64 KiB each).",
		Destructive: true,
	},
	{
		Name:        "sandbox_logs",
		Description: "Fetch trailing log lines from a GVX sandbox by ID or name (default 100 lines, max 2000, truncated to 64 KiB).",
		ReadOnly:    true,
	},
	{
		Name:        "create_snapshot",
		Description: "Take a fast gVisor checkpoint snapshot of a running GVX sandbox (leaves the sandbox running by default).",
	},
	{
		Name:        "list_snapshots",
		Description: "List gVisor checkpoint snapshots, optionally filtered to a single sandbox by ID or name.",
		ReadOnly:    true,
	},
	{
		Name:        "restore_snapshot",
		Description: "Restore/rollback a GVX sandbox to a previously saved checkpoint snapshot.",
		Destructive: true,
	},
	{
		Name:        "list_containers",
		Description: "List Docker containers managed by GVX Desktop (includes stopped containers by default).",
		ReadOnly:    true,
	},
	{
		Name:        "container_logs",
		Description: "Fetch trailing log lines from a Docker container by ID or name (default 100 lines, max 2000, truncated to 64 KiB).",
		ReadOnly:    true,
	},
	{
		Name:        "list_images",
		Description: "List container images available in GVX Desktop ('all', 'cached', or 'registry').",
		ReadOnly:    true,
	},
	{
		Name:        "list_routes",
		Description: "List active *.localhost local HTTP/HTTPS reverse proxy routes.",
		ReadOnly:    true,
	},
}

// ToolNames returns the canonical ordered list of MCP tool names exposed by Server.
func ToolNames() []string {
	names := make([]string, len(toolCatalog))
	for i, t := range toolCatalog {
		names[i] = t.Name
	}
	return names
}

// ToolSummaries returns metadata for all MCP tools exposed by Server.
func ToolSummaries() []ToolSummary {
	out := make([]ToolSummary, len(toolCatalog))
	copy(out, toolCatalog)
	return out
}

// Typed tool input / output structs used by mcp.AddTool for schema inference.

type emptyInput struct{}

type sandboxTargetInput struct {
	Target string `json:"target" jsonschema:"Sandbox ID or name."`
}

type execInSandboxInput struct {
	Target  string   `json:"target" jsonschema:"Running sandbox ID or name."`
	Command []string `json:"command" jsonschema:"Command and arguments to execute as an argv array (e.g. ['sh', '-c', 'ls -la'])."`
	Workdir string   `json:"workdir,omitempty" jsonschema:"Optional working directory inside the sandbox."`
	Env     []string `json:"env,omitempty" jsonschema:"Optional additional environment variables in KEY=VALUE format."`
}

type sandboxLogsInput struct {
	Target string `json:"target" jsonschema:"Sandbox ID or name."`
	Tail   int    `json:"tail,omitempty" jsonschema:"Number of trailing log lines to return (default: 100, max: 2000)."`
}

type createSnapshotInput struct {
	Target       string `json:"target" jsonschema:"Running sandbox ID or name to checkpoint."`
	LeaveRunning *bool  `json:"leave_running,omitempty" jsonschema:"Keep the sandbox running after taking the snapshot (default: true)."`
}

type listSnapshotsInput struct {
	Target string `json:"target,omitempty" jsonschema:"Optional sandbox ID or name to filter snapshots; omit to list snapshots for all sandboxes."`
}

type restoreSnapshotInput struct {
	Target   string `json:"target" jsonschema:"Sandbox ID or name to restore."`
	Snapshot string `json:"snapshot" jsonschema:"Name of the checkpoint snapshot to restore."`
}

type listContainersInput struct {
	All *bool `json:"all,omitempty" jsonschema:"Include stopped containers in addition to running ones (default: true)."`
}

type containerLogsInput struct {
	Target string `json:"target" jsonschema:"Docker container ID or name."`
	Tail   int    `json:"tail,omitempty" jsonschema:"Number of trailing log lines to return (default: 100, max: 2000)."`
}

type listImagesInput struct {
	Source string `json:"source,omitempty" jsonschema:"Image source filter: 'all' (default), 'cached', or 'registry'."`
}

type listSandboxesOutput struct {
	Sandboxes []SandboxInfo `json:"sandboxes" jsonschema:"List of GVX sandboxes."`
	Count     int           `json:"count" jsonschema:"Total number of sandboxes returned."`
}

type deleteSandboxOutput struct {
	Deleted bool   `json:"deleted" jsonschema:"Whether the sandbox was deleted."`
	Target  string `json:"target" jsonschema:"Sandbox ID or name that was deleted."`
}

type logsOutput struct {
	Target    string `json:"target" jsonschema:"Sandbox or container ID or name."`
	Tail      int    `json:"tail" jsonschema:"Number of trailing lines requested."`
	Logs      string `json:"logs" jsonschema:"Captured log output (truncated to 64 KiB if larger)."`
	Truncated bool   `json:"truncated" jsonschema:"Whether log output was truncated to fit MaxOutputBytes."`
}

type listSnapshotsOutput struct {
	Snapshots []SnapshotInfo `json:"snapshots" jsonschema:"List of checkpoint snapshots."`
	Count     int            `json:"count" jsonschema:"Total number of snapshots returned."`
}

type listContainersOutput struct {
	Containers []ContainerInfo `json:"containers" jsonschema:"List of Docker containers."`
	Count      int             `json:"count" jsonschema:"Total number of containers returned."`
}

type listImagesOutput struct {
	Images []ImageInfo `json:"images" jsonschema:"List of container images."`
	Count  int         `json:"count" jsonschema:"Total number of images returned."`
}

type listRoutesOutput struct {
	Routes []RouteInfo `json:"routes" jsonschema:"List of active *.localhost routes."`
	Count  int         `json:"count" jsonschema:"Total number of routes returned."`
}

func boolPtr(v bool) *bool { return &v }

func normalizeTail(tail int) int {
	if tail <= 0 {
		return DefaultLogTail
	}
	if tail > MaxLogTail {
		return MaxLogTail
	}
	return tail
}

// truncateOutput caps s to at most maxBytes bytes, preserving UTF-8 rune
// boundaries and appending truncatedMarker when truncation occurs.
func truncateOutput(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	if maxBytes <= len(truncatedMarker) {
		return truncatedMarker[:maxBytes], true
	}
	cut := maxBytes - len(truncatedMarker)
	minCut := max(0, cut-utf8.UTFMax+1)
	for cut > minCut && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker, true
}

func (s *Server) requireBackend() (Backend, error) {
	if s.backend == nil {
		return nil, errNoBackend
	}
	return s.backend, nil
}

func (s *Server) registerTools(srv *mcp.Server) {
	// 1. list_sandboxes
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_sandboxes",
		Title:       "List Sandboxes",
		Description: toolCatalog[0].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "List Sandboxes",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, listSandboxesOutput, error) {
		var out listSandboxesOutput
		err := s.runTool("list_sandboxes", func(b Backend) error {
			items, err := b.ListSandboxes(ctx)
			if err != nil {
				return err
			}
			if items == nil {
				items = []SandboxInfo{}
			}
			out = listSandboxesOutput{Sandboxes: items, Count: len(items)}
			return nil
		})
		return nil, out, err
	})

	// 2. create_sandbox
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_sandbox",
		Title:       "Create Sandbox",
		Description: toolCatalog[1].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Create Sandbox",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(true),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CreateSandboxRequest) (*mcp.CallToolResult, SandboxInfo, error) {
		var out SandboxInfo
		err := s.runTool("create_sandbox", func(b Backend) error {
			in.Image = strings.TrimSpace(in.Image)
			if in.Image == "" {
				return errors.New("image is required")
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Checkpoint == nil {
				in.Checkpoint = boolPtr(true)
			}
			if in.Start == nil {
				in.Start = boolPtr(true)
			}
			info, err := b.CreateSandbox(ctx, in)
			if err != nil {
				return err
			}
			out = info
			return nil
		})
		return nil, out, err
	})

	// 3. start_sandbox
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "start_sandbox",
		Title:       "Start Sandbox",
		Description: toolCatalog[2].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Start Sandbox",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sandboxTargetInput) (*mcp.CallToolResult, SandboxInfo, error) {
		var out SandboxInfo
		err := s.runTool("start_sandbox", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			info, err := b.StartSandbox(ctx, target)
			if err != nil {
				return err
			}
			out = info
			return nil
		})
		return nil, out, err
	})

	// 4. stop_sandbox
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "stop_sandbox",
		Title:       "Stop Sandbox",
		Description: toolCatalog[3].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Stop Sandbox",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sandboxTargetInput) (*mcp.CallToolResult, SandboxInfo, error) {
		var out SandboxInfo
		err := s.runTool("stop_sandbox", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			info, err := b.StopSandbox(ctx, target)
			if err != nil {
				return err
			}
			out = info
			return nil
		})
		return nil, out, err
	})

	// 5. delete_sandbox
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_sandbox",
		Title:       "Delete Sandbox",
		Description: toolCatalog[4].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Delete Sandbox",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sandboxTargetInput) (*mcp.CallToolResult, deleteSandboxOutput, error) {
		var out deleteSandboxOutput
		err := s.runTool("delete_sandbox", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			if err := b.DeleteSandbox(ctx, target); err != nil {
				return err
			}
			out = deleteSandboxOutput{Deleted: true, Target: target}
			return nil
		})
		return nil, out, err
	})

	// 6. exec_in_sandbox
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "exec_in_sandbox",
		Title:       "Execute in Sandbox",
		Description: toolCatalog[5].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Execute in Sandbox",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(true),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in execInSandboxInput) (*mcp.CallToolResult, ExecResult, error) {
		var out ExecResult
		err := s.runTool("exec_in_sandbox", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			if len(in.Command) == 0 || strings.TrimSpace(in.Command[0]) == "" {
				return errors.New("command must contain at least one executable argument")
			}
			res, err := b.ExecInSandbox(ctx, target, in.Command, strings.TrimSpace(in.Workdir), in.Env)
			if err != nil {
				return err
			}
			res.Stdout, _ = truncateOutput(res.Stdout, MaxOutputBytes)
			res.Stderr, _ = truncateOutput(res.Stderr, MaxOutputBytes)
			out = res
			return nil
		})
		return nil, out, err
	})

	// 7. sandbox_logs
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "sandbox_logs",
		Title:       "Sandbox Logs",
		Description: toolCatalog[6].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "Sandbox Logs",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sandboxLogsInput) (*mcp.CallToolResult, logsOutput, error) {
		var out logsOutput
		err := s.runTool("sandbox_logs", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			tail := normalizeTail(in.Tail)
			raw, err := b.SandboxLogs(ctx, target, tail)
			if err != nil {
				return err
			}
			trimmed, truncated := truncateOutput(raw, MaxOutputBytes)
			out = logsOutput{
				Target:    target,
				Tail:      tail,
				Logs:      trimmed,
				Truncated: truncated,
			}
			return nil
		})
		return nil, out, err
	})

	// 8. create_snapshot
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_snapshot",
		Title:       "Create Snapshot",
		Description: toolCatalog[7].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Create Snapshot",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createSnapshotInput) (*mcp.CallToolResult, SnapshotInfo, error) {
		var out SnapshotInfo
		err := s.runTool("create_snapshot", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			leaveRunning := true
			if in.LeaveRunning != nil {
				leaveRunning = *in.LeaveRunning
			}
			info, err := b.CreateSnapshot(ctx, target, leaveRunning)
			if err != nil {
				return err
			}
			out = info
			return nil
		})
		return nil, out, err
	})

	// 9. list_snapshots
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_snapshots",
		Title:       "List Snapshots",
		Description: toolCatalog[8].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "List Snapshots",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listSnapshotsInput) (*mcp.CallToolResult, listSnapshotsOutput, error) {
		var out listSnapshotsOutput
		err := s.runTool("list_snapshots", func(b Backend) error {
			items, err := b.ListSnapshots(ctx, strings.TrimSpace(in.Target))
			if err != nil {
				return err
			}
			if items == nil {
				items = []SnapshotInfo{}
			}
			out = listSnapshotsOutput{Snapshots: items, Count: len(items)}
			return nil
		})
		return nil, out, err
	})

	// 10. restore_snapshot
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "restore_snapshot",
		Title:       "Restore Snapshot",
		Description: toolCatalog[9].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:           "Restore Snapshot",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in restoreSnapshotInput) (*mcp.CallToolResult, SandboxInfo, error) {
		var out SandboxInfo
		err := s.runTool("restore_snapshot", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			snapshot := strings.TrimSpace(in.Snapshot)
			if snapshot == "" {
				return errors.New("snapshot is required")
			}
			info, err := b.RestoreSnapshot(ctx, target, snapshot)
			if err != nil {
				return err
			}
			out = info
			return nil
		})
		return nil, out, err
	})

	// 11. list_containers
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_containers",
		Title:       "List Containers",
		Description: toolCatalog[10].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "List Containers",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listContainersInput) (*mcp.CallToolResult, listContainersOutput, error) {
		var out listContainersOutput
		err := s.runTool("list_containers", func(b Backend) error {
			all := true
			if in.All != nil {
				all = *in.All
			}
			items, err := b.ListContainers(ctx, all)
			if err != nil {
				return err
			}
			if items == nil {
				items = []ContainerInfo{}
			}
			out = listContainersOutput{Containers: items, Count: len(items)}
			return nil
		})
		return nil, out, err
	})

	// 12. container_logs
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "container_logs",
		Title:       "Container Logs",
		Description: toolCatalog[11].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "Container Logs",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in containerLogsInput) (*mcp.CallToolResult, logsOutput, error) {
		var out logsOutput
		err := s.runTool("container_logs", func(b Backend) error {
			target := strings.TrimSpace(in.Target)
			if target == "" {
				return errors.New("target is required")
			}
			tail := normalizeTail(in.Tail)
			raw, err := b.ContainerLogs(ctx, target, tail)
			if err != nil {
				return err
			}
			trimmed, truncated := truncateOutput(raw, MaxOutputBytes)
			out = logsOutput{
				Target:    target,
				Tail:      tail,
				Logs:      trimmed,
				Truncated: truncated,
			}
			return nil
		})
		return nil, out, err
	})

	// 13. list_images
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_images",
		Title:       "List Images",
		Description: toolCatalog[12].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "List Images",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listImagesInput) (*mcp.CallToolResult, listImagesOutput, error) {
		var out listImagesOutput
		err := s.runTool("list_images", func(b Backend) error {
			source := strings.ToLower(strings.TrimSpace(in.Source))
			if source == "" {
				source = "all"
			}
			switch source {
			case "all", "cached", "registry":
			default:
				return fmt.Errorf("invalid source %q: expected \"all\", \"cached\", or \"registry\"", in.Source)
			}
			items, err := b.ListImages(ctx, source)
			if err != nil {
				return err
			}
			if items == nil {
				items = []ImageInfo{}
			}
			out = listImagesOutput{Images: items, Count: len(items)}
			return nil
		})
		return nil, out, err
	})

	// 14. list_routes
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_routes",
		Title:       "List Routes",
		Description: toolCatalog[13].Description,
		Annotations: &mcp.ToolAnnotations{
			Title:         "List Routes",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, listRoutesOutput, error) {
		var out listRoutesOutput
		err := s.runTool("list_routes", func(b Backend) error {
			items, err := b.ListRoutes(ctx)
			if err != nil {
				return err
			}
			if items == nil {
				items = []RouteInfo{}
			}
			out = listRoutesOutput{Routes: items, Count: len(items)}
			return nil
		})
		return nil, out, err
	})
}

func (s *Server) runTool(name string, fn func(Backend) error) error {
	b, err := s.requireBackend()
	if err == nil {
		err = fn(b)
	}
	s.recordCall(name, err)
	return err
}
