package engine

// Owner: frozen (architect). The amd64 builder's shared types and seam
// signatures; their bodies are in amd64.go (workstream C). See
// docs/design/amd64-builder.md.

import (
	"context"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
)

const (
	// Amd64NodeName is the gvx-desktop buildx builder's node that the amd64
	// builder VM serves, appended with --platform linux/amd64.
	Amd64NodeName = "gvx-desktop-amd64"
	// Amd64Platform is the one platform buildx sends to that node.
	Amd64Platform = "linux/amd64"

	// amd64SocketName is the node's lazy front socket, beside buildkit.sock.
	amd64SocketName = "buildkit-amd64.sock"
	// amd64DirName is the amd64 builder's directory under Config.Dir: the
	// helper's state (its socket, its log, the console log) and the disk.
	amd64DirName = "amd64"
	// amd64DiskName is the persistent disk image in that directory.
	amd64DiskName = "disk.img"

	defaultAmd64DiskSize        = 64 << 30 // sparse: only what buildkit writes takes room
	defaultAmd64MemoryMiB       = 4096
	defaultAmd64IdleTimeout     = 10 * time.Minute
	defaultAmd64ProbeIdle       = 30 * time.Second
	amd64ProbeBytes             = 64 << 10 // less through all connections since the start is a probe
	defaultAmd64StartTimeout    = 2 * time.Minute
	defaultAmd64HelperStopGrace = 15 * time.Second
)

// Amd64Config turns on the amd64 builder: a second node of the gvx-desktop
// builder, served by a Virtualization.framework VM with Rosetta that a
// helper process runs. Zero fields take the defaults.
type Amd64Config struct {
	// Launcher starts the helper; its Dir is left empty here, the engine
	// sets it (<Config.Dir>/amd64, or a short temporary directory when that
	// is too long for a socket path).
	Launcher vmclient.Launcher
	// Assets finds the VM's assets: vmassets.Resolve with the app's
	// options. It may download; the engine calls it on the first build,
	// with progress in Amd64State.
	Assets func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error)
	// DiskPath is the persistent disk image; empty is
	// <Config.Dir>/amd64/disk.img. DiskSize is its size when made.
	DiskPath string
	DiskSize int64
	// CPUs defaults to half the host's, at least 2; MemoryMiB to 4096.
	CPUs      int
	MemoryMiB int
	// IdleTimeout is how long the VM runs after its last connection
	// closed; ProbeIdleTimeout replaces it while everything since the
	// start was a probe (buildx ls and inspect connect to every node).
	IdleTimeout      time.Duration
	ProbeIdleTimeout time.Duration
}

// Amd64State is the amd64 builder's state, for Settings.
type Amd64State struct {
	// Enabled is whether a config is set (SetAmd64 with non-nil).
	Enabled bool
	// Status is BuilderIdle, BuilderStarting, BuilderRunning or
	// Amd64Stopping.
	Status string
	// Err is the last start's or the VM's failure, cleared on the next
	// start.
	Err string
	// Downloading is set while the assets download; Done and Total count
	// bytes.
	Downloading bool
	Done, Total int64
	// Assets is the asset set in use ("1", or "local" for an override).
	Assets string
	// DiskBytes is how much of the disk image the host has allocated.
	DiskBytes int64
}

// Amd64Stopping is Amd64State.Status while the VM shuts down.
const Amd64Stopping = "stopping"

// Equal makes Amd64State comparable for signals.
func (s Amd64State) Equal(o Amd64State) bool { return s == o }
