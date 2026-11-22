// Package sandbox describes the sandboxes the desktop app keeps and runs
// them as supervised rungv processes.
package sandbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// MountType names the kind of a mount.
type MountType string

const (
	MountVolume MountType = "volume" // a named ext4lite volume, or an .ext4 file at a path
	MountBind   MountType = "bind"   // a host directory or file
	MountTmpfs  MountType = "tmpfs"  // memory-backed
)

// Mount is one mount of a sandbox.
type Mount struct {
	Type     MountType `json:"type"`
	Source   string    `json:"source,omitempty"` // volume name, image path, or host path
	Target   string    `json:"target"`
	ReadOnly bool      `json:"readOnly,omitempty"`
	Size     string    `json:"size,omitempty"` // tmpfs size, such as 64m
}

// Spec is everything a sandbox starts with. Because a gvx sandbox locks its
// host process down before the guest runs, every port and mount here is
// fixed for the life of a run.
type Spec struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	// Command replaces the image Cmd, as a shell-quoted line.
	Command    string `json:"command,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
	// SetEntrypoint makes Entrypoint apply even when empty, which clears it.
	SetEntrypoint bool `json:"setEntrypoint,omitempty"`

	Env   []string `json:"env,omitempty"`   // KEY=VALUE, or KEY to copy from the host
	Ports []string `json:"ports,omitempty"` // rungv -p specs

	Mounts       []Mount `json:"mounts,omitempty"`
	Workspace    string  `json:"workspace,omitempty"`
	WorkspaceRO  bool    `json:"workspaceRO,omitempty"`
	RootfsVolume string  `json:"rootfsVolume,omitempty"`

	Memory string `json:"memory,omitempty"` // 512m, 2g
	CPUs   string `json:"cpus,omitempty"`   // 1.5

	Network    string `json:"network,omitempty"` // nat (default), none, wg
	WGConf     string `json:"wgConf,omitempty"`
	IsolateLAN bool   `json:"isolateLAN,omitempty"`
	Backend    string `json:"backend,omitempty"` // auto (default), hvf, whpx, systrap

	Hostname string `json:"hostname,omitempty"`
	User     string `json:"user,omitempty"`
	Workdir  string `json:"workdir,omitempty"`

	// Unprivileged turns --privileged off. rungv keeps it on by default,
	// because its privileges stay inside the sandbox.
	Unprivileged bool   `json:"unprivileged,omitempty"`
	HostCACerts  bool   `json:"hostCACerts,omitempty"`
	Pull         string `json:"pull,omitempty"` // missing (default), always, never

	// Checkpoint pre-opens a checkpoint directory and a control socket so
	// the running sandbox can be snapshotted.
	Checkpoint bool `json:"checkpoint,omitempty"`
	AutoStart  bool `json:"autoStart,omitempty"`

	// Recipe names the preset the sandbox came from: docker, k3s, or "".
	Recipe string `json:"recipe,omitempty"`
	// ReadyPattern is a log substring that marks the guest as ready.
	ReadyPattern string `json:"readyPattern,omitempty"`

	ExtraArgs string    `json:"extraArgs,omitempty"` // more rungv run flags, shell-quoted
	CreatedAt time.Time `json:"createdAt"`

	// Interactive gives the run a standard input that Supervisor.Stdin
	// writes to (rungv -i), in place of the empty one that stays open.
	Interactive bool `json:"interactive,omitempty"`
	// TTY runs the guest on a pseudo-terminal (rungv -t) that the
	// supervisor holds the other side of.
	TTY bool `json:"tty,omitempty"`
	// ContainerID marks a sandbox the app's Docker engine runs for a
	// container: it is not one of the user's sandboxes.
	ContainerID string `json:"containerId,omitempty"`
}

// NewID returns a short random identifier.
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var (
	nameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	volumeRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	memoryRe = regexp.MustCompile(`^(?i)\d+(\.\d+)?[bkmgt]?$`)
)

// SuggestName derives a sandbox name from an image reference.
func SuggestName(image string) string {
	name := image
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, "-._")
	if name == "" {
		return "sandbox"
	}
	return name
}

// Validate reports the first problem that would stop the sandbox.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("Give the sandbox a name.")
	}
	if !nameRe.MatchString(s.Name) {
		return errors.New("Use letters, digits, '-', '_' and '.' in the name.")
	}
	if strings.TrimSpace(s.Image) == "" {
		return errors.New("Choose an image to run.")
	}
	if strings.ContainsAny(s.Image, " \t") {
		return errors.New("An image reference has no spaces.")
	}
	if _, err := rungv.SplitArgs(s.Command); err != nil {
		return fmt.Errorf("Command: %v.", err)
	}
	if _, err := rungv.SplitArgs(s.ExtraArgs); err != nil {
		return fmt.Errorf("Extra flags: %v.", err)
	}
	for _, p := range s.Ports {
		if err := ValidatePort(p); err != nil {
			return err
		}
	}
	targets := map[string]bool{}
	if s.Workspace != "" {
		targets["/workspace"] = true
	}
	for _, m := range s.Mounts {
		if err := m.validate(); err != nil {
			return err
		}
		if targets[m.Target] {
			return fmt.Errorf("Two mounts share the guest path %s.", m.Target)
		}
		targets[m.Target] = true
	}
	if s.RootfsVolume != "" {
		for _, m := range s.Mounts {
			if m.Type == MountVolume && m.Source == s.RootfsVolume {
				return errors.New("The root file system volume cannot also be mounted as a data volume.")
			}
		}
	}
	if s.Memory != "" && !memoryRe.MatchString(s.Memory) {
		return errors.New("Write the memory limit as a number with a unit, such as 512m or 2g.")
	}
	if s.CPUs != "" {
		if v, err := strconv.ParseFloat(s.CPUs, 64); err != nil || v <= 0 {
			return errors.New("Write the CPU limit as a positive number, such as 1.5.")
		}
	}
	for _, e := range s.Env {
		if k, _, _ := strings.Cut(e, "="); strings.TrimSpace(k) == "" || strings.ContainsAny(k, " \t") {
			return fmt.Errorf("Environment entry %q needs a KEY.", e)
		}
	}
	if s.Network == "wg" && s.WGConf == "" {
		return errors.New("The WireGuard network needs a configuration file.")
	}
	if (s.Network == "none" || s.Network == "wg") && len(s.Ports) > 0 {
		return errors.New("Published ports need the NAT network.")
	}
	return nil
}

func (m Mount) validate() error {
	if !strings.HasPrefix(m.Target, "/") || m.Target == "/" {
		return fmt.Errorf("Mount target %q must be an absolute guest path other than /.", m.Target)
	}
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		if (m.Target == p || strings.HasPrefix(m.Target, p+"/")) && !strings.HasPrefix(m.Target, "/dev/shm") {
			return fmt.Errorf("Mount target %s is reserved by the guest kernel.", m.Target)
		}
	}
	switch m.Type {
	case MountVolume:
		if m.Source == "" {
			return fmt.Errorf("The volume at %s needs a name.", m.Target)
		}
		if !strings.ContainsAny(m.Source, `/\`) && !volumeRe.MatchString(m.Source) {
			return fmt.Errorf("Volume name %q: use letters, digits, '-', '_' and '.'.", m.Source)
		}
	case MountBind:
		if m.Source == "" {
			return fmt.Errorf("The bind mount at %s needs a host path.", m.Target)
		}
	case MountTmpfs:
	default:
		return fmt.Errorf("Unknown mount type %q.", m.Type)
	}
	return nil
}

var portRe = regexp.MustCompile(`^(?:(\[[0-9a-fA-F:.]+\]|\d{1,3}(?:\.\d{1,3}){3}):)?(?:(\d+(?:-\d+)?):)?(\d+(?:-\d+)?)(?:/(tcp|udp))?$`)

// ValidatePort checks a -p specification.
func ValidatePort(p string) error {
	p = strings.TrimSpace(p)
	if after, ok := strings.CutPrefix(p, "unix:"); ok {
		rest := after
		i := strings.LastIndexByte(rest, ':')
		if i <= 0 {
			return fmt.Errorf("Port %q: write unix:PATH:GUEST_PORT.", p)
		}
		if _, err := strconv.Atoi(rest[i+1:]); err != nil {
			return fmt.Errorf("Port %q: the guest port must be a number.", p)
		}
		if strings.Contains(rest, ",") {
			return fmt.Errorf("Port %q: a socket path cannot contain a comma.", p)
		}
		return nil
	}
	m := portRe.FindStringSubmatch(p)
	if m == nil {
		return fmt.Errorf("Port %q: write [IP:][HOST:]GUEST[/udp], such as 8080:80.", p)
	}
	host, guest := m[2], m[3]
	gl, err := rangeLen(guest)
	if err != nil {
		return fmt.Errorf("Port %q: %v", p, err)
	}
	if host != "" {
		hl, err := rangeLen(host)
		if err != nil {
			return fmt.Errorf("Port %q: %v", p, err)
		}
		if hl != gl {
			return fmt.Errorf("Port %q: the host and guest ranges must have the same length.", p)
		}
	}
	return nil
}

func rangeLen(s string) (int, error) {
	lo, hi, isRange := strings.Cut(s, "-")
	a, err := strconv.Atoi(lo)
	if err != nil || a > 65535 {
		return 0, errors.New("ports run from 0 to 65535.")
	}
	if !isRange {
		return 1, nil
	}
	b, err := strconv.Atoi(hi)
	if err != nil || b > 65535 || b < a {
		return 0, errors.New("a range runs from a low port to a higher one.")
	}
	if b-a+1 > 1024 {
		return 0, errors.New("one range holds at most 1024 ports.")
	}
	return b - a + 1, nil
}

// Paths are the host files a run of a sandbox uses besides its spec.
type Paths struct {
	// ControlSocket is set when the spec has Checkpoint (as with rungv
	// v0.19) and, with a rungv whose Features have Control, for every run:
	// passed as -control-socket. See docs/design/gvx-v020.md §4.1.
	ControlSocket string
	// TokenFile is set with ControlSocket where the socket needs a token
	// (a Unix socket on Windows): --control-token-file on run and restore,
	// --token-file on every client command (rungv.Target). Owner: W1.
	TokenFile string
	// Control reports that the run's socket speaks gvx-ctrl/2: the rungv
	// that starts it has Features.Control. Owner: W1.
	Control       bool
	CheckpointDir string // set when the spec has Checkpoint
}

// RunArgs is the argument vector of rungv run for the spec, without the
// rungv executable itself.
func (s Spec) RunArgs(p Paths) []string {
	args := []string{"run"}
	args = append(args, s.flags(p, false)...)
	args = append(args, s.Image)
	cmd, _ := rungv.SplitArgs(s.Command)
	return append(args, cmd...)
}

// RestoreArgs resumes the checkpoint bundle in dir with the same mounts,
// ports and network the bundle was written with.
func (s Spec) RestoreArgs(dir string, p Paths) []string {
	args := []string{"restore", "-checkpoint-dir", dir}
	// A restore takes a new listener and directory for the next snapshot.
	if p.ControlSocket != "" {
		args = append(args, "-control-socket", p.ControlSocket)
	}
	if p.TokenFile != "" {
		args = append(args, "--control-token-file", p.TokenFile)
	}
	return append(args, s.flags(Paths{}, true)...)
}

func (s Spec) flags(p Paths, restore bool) []string {
	var a []string
	add := func(v ...string) { a = append(a, v...) }
	// The name is the guest's container ID, which a restore needs to match
	// the bundle's host mounts.
	if s.Name != "" {
		add("--name", s.Name)
	}
	if !restore {
		if s.Hostname != "" {
			add("--hostname", s.Hostname)
		}
		if s.User != "" {
			add("--user", s.User)
		}
		if s.SetEntrypoint || s.Entrypoint != "" {
			add("--entrypoint", s.Entrypoint)
		}
		for _, e := range s.Env {
			add("-e", e)
		}
		if s.Memory != "" {
			add("--memory", s.Memory)
		}
		if s.CPUs != "" {
			add("--cpus", s.CPUs)
		}
		if s.Backend != "" && s.Backend != "auto" {
			add("--backend", s.Backend)
		}
		if s.Unprivileged {
			add("--privileged=false")
		}
		if s.HostCACerts {
			add("--host-ca-certs")
		}
		if s.Pull != "" && s.Pull != "missing" {
			add("--pull", s.Pull)
		}
		if s.Interactive {
			add("-i")
		}
		if s.TTY {
			add("-t")
		}
	}
	if s.Workdir != "" {
		add("--workdir", s.Workdir)
	}
	switch s.Network {
	case "", "nat":
	default:
		add("-net", s.Network)
	}
	if s.Network == "wg" && s.WGConf != "" {
		add("-wg-conf", rungv.ExpandHome(s.WGConf))
	}
	if s.IsolateLAN {
		add("-isolate-lan")
	}
	for _, port := range s.Ports {
		add("-p", strings.TrimSpace(port))
	}
	if s.Workspace != "" {
		add("-workspace", rungv.ExpandHome(s.Workspace))
		if s.WorkspaceRO {
			add("-ro")
		}
	}
	if s.RootfsVolume != "" {
		add("-rootfs-volume", rungv.ExpandHome(s.RootfsVolume))
	}
	for _, m := range s.Mounts {
		add(m.args()...)
	}
	if !restore && p.ControlSocket != "" {
		add("-control-socket", p.ControlSocket)
	}
	if !restore && p.TokenFile != "" {
		add("--control-token-file", p.TokenFile)
	}
	if !restore && p.CheckpointDir != "" {
		add("-checkpoint-dir", p.CheckpointDir)
	}
	if !restore {
		extra, _ := rungv.SplitArgs(s.ExtraArgs)
		a = append(a, extra...)
	}
	return a
}

func (m Mount) args() []string {
	switch m.Type {
	case MountTmpfs:
		v := m.Target
		var opts []string
		if m.Size != "" {
			opts = append(opts, "size="+m.Size)
		}
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		if len(opts) > 0 {
			v += ":" + strings.Join(opts, ",")
		}
		return []string{"--tmpfs", v}
	case MountVolume:
		if strings.ContainsAny(m.Source, `/\`) {
			// An .ext4 image at a host path goes through --mount, because -v
			// would mount the file itself as one regular file.
			v := "type=volume,src=" + rungv.ExpandHome(m.Source) + ",dst=" + m.Target
			if m.ReadOnly {
				v += ",readonly"
			}
			return []string{"--mount", v}
		}
		return []string{"-v", volumeSpec(m.Source, m.Target, m.ReadOnly)}
	default:
		return []string{"-v", volumeSpec(rungv.ExpandHome(m.Source), m.Target, m.ReadOnly)}
	}
}

func volumeSpec(src, dst string, ro bool) string {
	v := src + ":" + dst
	if ro {
		v += ":ro"
	}
	return v
}

// CommandLine is the run command as one line a person could paste into a
// shell.
func (s Spec) CommandLine(p Paths) string {
	return "rungv " + rungv.FormatArgs(s.RunArgs(p))
}

// Prepare creates the host directories a run needs before rungv starts:
// the parent of each published Unix socket. rungv creates missing bind
// directories and volume images itself.
func (s Spec) Prepare() error {
	for _, port := range s.Ports {
		if !strings.HasPrefix(port, "unix:") {
			continue
		}
		rest := strings.TrimPrefix(port, "unix:")
		i := strings.LastIndexByte(rest, ':')
		if i <= 0 {
			continue
		}
		dir := filepath.Dir(rungv.ExpandHome(rest[:i]))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Volumes lists the named volumes the spec mounts, its root volume
// included.
func (s Spec) Volumes() []string {
	var out []string
	if s.RootfsVolume != "" && !strings.ContainsAny(s.RootfsVolume, `/\`) {
		out = append(out, s.RootfsVolume)
	}
	for _, m := range s.Mounts {
		if m.Type == MountVolume && !strings.ContainsAny(m.Source, `/\`) {
			out = append(out, m.Source)
		}
	}
	return out
}

// UnixSockets lists the expanded host socket paths the spec publishes.
func (s Spec) UnixSockets() []string {
	var out []string
	for _, port := range s.Ports {
		if rest, ok := strings.CutPrefix(port, "unix:"); ok {
			if i := strings.LastIndexByte(rest, ':'); i > 0 {
				out = append(out, rungv.ExpandHome(rest[:i]))
			}
		}
	}
	return out
}

// Clone returns a deep copy of the spec.
func (s Spec) Clone() Spec {
	c := s
	c.Env = append([]string(nil), s.Env...)
	c.Ports = append([]string(nil), s.Ports...)
	c.Mounts = append([]Mount(nil), s.Mounts...)
	return c
}

// Equal compares two specs field by field; a nil and an empty list are
// the same.
func (s Spec) Equal(o Spec) bool {
	a, errA := json.Marshal(s)
	b, errB := json.Marshal(o)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// autoPublish is the rungv flag that publishes on 127.0.0.1, under the same
// number, every TCP port the guest comes to listen on or forward, and
// takes it back when the guest stops: what a sandbox whose ports are not
// known when it starts needs, such as a Docker Engine.
const autoPublish = "-auto-publish"

// isAutoPublish reports whether arg is the flag, however it is written, and
// what it is set to.
func isAutoPublish(arg string) (on, ok bool) {
	name, value, valued := strings.Cut(strings.TrimPrefix(arg, "-"), "=")
	if name != autoPublish && name != autoPublish[1:] || !strings.HasPrefix(arg, "-") {
		return false, false
	}
	return !valued || value != "false" && value != "0", true
}

// AutoPublishSet reports whether the spec's extra flags say anything about
// auto-publishing, on or off.
func AutoPublishSet(s Spec) bool {
	extra, _ := rungv.SplitArgs(s.ExtraArgs)
	for _, a := range extra {
		if _, ok := isAutoPublish(a); ok {
			return true
		}
	}
	return false
}

// HasAutoPublish reports whether the spec runs with -auto-publish.
func HasAutoPublish(s Spec) bool {
	extra, _ := rungv.SplitArgs(s.ExtraArgs)
	on := false
	for _, a := range extra {
		if v, ok := isAutoPublish(a); ok {
			on = v
		}
	}
	return on
}

// WithAutoPublish returns the spec with -auto-publish among its extra
// flags, or without it.
func WithAutoPublish(s Spec, on bool) Spec {
	extra, err := rungv.SplitArgs(s.ExtraArgs)
	if err != nil {
		return s
	}
	out := make([]string, 0, len(extra)+1)
	for _, a := range extra {
		if _, ok := isAutoPublish(a); !ok {
			out = append(out, a)
		}
	}
	if on {
		out = append(out, autoPublish)
	}
	s = s.Clone()
	s.ExtraArgs = rungv.FormatArgs(out)
	return s
}

// dockerdStopGrace is what dockerd takes at most to end: it gives its
// containers 15 seconds (its shutdown-timeout) to stop, then cleans up.
const dockerdStopGrace = 20 * time.Second

// gracefulStop reports whether a sandbox ends with SIGTERM to its first
// process and time to act on it, as docker stop ends a container, rather
// than at once: a recipe's, whose daemon keeps state (dockerd, k3s, the
// registry). grace is how long it gets: timeout, and for dockerd at least
// what it takes itself.
func (s Spec) gracefulStop(timeout time.Duration) (grace time.Duration, ok bool) {
	switch s.Recipe {
	case "":
		return 0, false
	case "docker": // recipes.Docker
		return max(timeout, dockerdStopGrace), true
	}
	return timeout, true
}
