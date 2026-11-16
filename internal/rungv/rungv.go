// Package rungv drives the gvx command line, rungv, as a child process.
//
// The desktop app never links gvx's Go SDK: gvx.New locks down the calling
// process before the guest starts, which would leave the GUI unable to open
// files, sockets or windows. Every sandbox therefore runs as a rungv process
// of its own, and the management commands are read through --format json.
package rungv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ErrNotFound reports that no rungv binary could be located.
var ErrNotFound = errors.New("rungv binary not found")

// BinaryName is the executable name of rungv on this host.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "rungv.exe"
	}
	return "rungv"
}

// Candidates lists the places Locate looks, in order. preferred, when set,
// comes first.
func Candidates(preferred string) []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
		}
	}
	add(ExpandHome(preferred))
	add(os.Getenv("GVX_RUNGV"))
	if p, err := exec.LookPath(BinaryName()); err == nil {
		add(p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".gvx", "bin", BinaryName()))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		add(filepath.Join(dir, BinaryName()))
		add(filepath.Join(dir, "..", "Resources", BinaryName())) // macOS .app bundle
		add(filepath.Join(dir, "bin", BinaryName()))
	}
	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, "bin", BinaryName())) // development checkout
	}
	if runtime.GOOS != "windows" {
		add("/opt/homebrew/bin/rungv")
		add("/usr/local/bin/rungv")
	}
	return out
}

// Locate returns the first executable candidate.
func Locate(preferred string) (string, error) {
	for _, p := range Candidates(preferred) {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return filepath.Clean(p), nil
	}
	return "", ErrNotFound
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Client runs rungv commands. The zero value is not usable; set Path.
type Client struct {
	Path     string
	CacheDir string // optional -cache-dir for every command that takes it
}

// Error is a failed rungv command with what it wrote to stderr.
type Error struct {
	Args   []string
	Code   int
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	// rungv prefixes its messages with the command name; keep the last line,
	// which carries the cause.
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[i+1:])
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// cacheFlagCommands take -cache-dir.
var cacheFlagCommands = map[string]bool{
	"run": true, "restore": true, "pull": true, "images": true, "inspect": true,
	"rmi": true, "tag": true, "save": true, "load": true, "image": true,
	"volume": true, "system": true, "info": true,
}

// Args returns the full argument vector for a command, adding -cache-dir
// where the command accepts it.
func (c *Client) Args(args ...string) []string {
	if c.CacheDir == "" || len(args) == 0 || !cacheFlagCommands[args[0]] {
		return args
	}
	out := make([]string, 0, len(args)+2)
	// Group commands take the flag after their verb.
	n := 1
	if (args[0] == "image" || args[0] == "volume" || args[0] == "system") && len(args) > 1 {
		n = 2
	}
	out = append(out, args[:n]...)
	out = append(out, "-cache-dir", c.CacheDir)
	return append(out, args[n:]...)
}

// Command builds an exec.Cmd for rungv with args.
func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Path, c.Args(args...)...)
	hideWindow(cmd)
	return cmd
}

func (c *Client) output(ctx context.Context, args ...string) ([]byte, error) {
	if c.Path == "" {
		return nil, ErrNotFound
	}
	cmd := c.Command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		e := &Error{Args: args, Stderr: stderr.String(), Err: err}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			e.Code = exit.ExitCode()
		}
		return stdout.Bytes(), e
	}
	return stdout.Bytes(), nil
}

// Stream runs a command and hands every line of stdout and stderr to line,
// from the goroutines reading them. It returns when the command exits.
func (c *Client) Stream(ctx context.Context, line func(string), args ...string) error {
	if c.Path == "" {
		return ErrNotFound
	}
	cmd := c.Command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var tail tailBuffer
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	read := func(r io.Reader, keep bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			text := sc.Text()
			if keep {
				tail.add(text)
			}
			line(text)
		}
	}
	wg.Add(2)
	go read(stdout, false)
	go read(stderr, true)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		e := &Error{Args: args, Stderr: tail.String(), Err: err}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			e.Code = exit.ExitCode()
		}
		return e
	}
	return nil
}

type tailBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (t *tailBuffer) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > 20 {
		t.lines = t.lines[len(t.lines)-20:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// Version is the parsed output of rungv version.
type Version struct {
	Version string // v0.16.0, (devel), …
	Commit  string
	Go      string
	Raw     string
}

// ParseVersion reads the three lines rungv version prints.
func ParseVersion(out string) Version {
	v := Version{Raw: strings.TrimSpace(out)}
	for _, line := range strings.Split(v.Raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "rungv":
			v.Version = f[1]
		case "commit":
			v.Commit = f[1]
		case "go":
			v.Go = strings.Join(f[1:], " ")
		}
	}
	return v
}

// Version runs rungv version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, err := c.output(ctx, "version")
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(string(out)), nil
}

// Info mirrors gvx.SystemInfo, the JSON of rungv info.
type Info struct {
	OS                string            `json:"OS"`
	Architecture      string            `json:"Architecture"`
	GoVersion         string            `json:"GoVersion"`
	NumCPU            int               `json:"NumCPU"`
	PageSize          int               `json:"PageSize"`
	DefaultBackend    string            `json:"DefaultBackend"`
	AvailableBackends []string          `json:"AvailableBackends"`
	BackendWarnings   []string          `json:"BackendWarnings,omitempty"`
	HostLockdown      string            `json:"HostLockdown"`
	LayerCacheDir     string            `json:"LayerCacheDir"`
	VolumeDir         string            `json:"VolumeDir"`
	Images            int               `json:"Images"`
	Volumes           int               `json:"Volumes"`
	BackendDetails    map[string]string `json:"BackendDetails,omitempty"`
}

// Info runs rungv info --format json.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	out, err := c.output(ctx, "info", "--format", "json")
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(out, &info)
	return info, err
}

// Image is one row of rungv images --format json.
type Image struct {
	CreatedAt    string `json:"CreatedAt"`
	CreatedSince string `json:"CreatedSince"`
	Digest       string `json:"Digest"`
	ID           string `json:"ID"`
	Repository   string `json:"Repository"`
	Size         string `json:"Size"`
	Tag          string `json:"Tag"`
}

// Ref is the reference that names the image: repository:tag, or the ID
// for a dangling one.
func (i Image) Ref() string {
	if i.Repository == "" || i.Repository == "<none>" {
		return i.ID
	}
	if i.Tag == "" || i.Tag == "<none>" {
		return i.Repository + "@" + i.Digest
	}
	return i.Repository + ":" + i.Tag
}

// Key identifies a row, unique even for an image with several tags.
func (i Image) Key() string { return i.ID + "|" + i.Repository + "|" + i.Tag }

// Volume is one row of rungv volume ls --format json.
type Volume struct {
	Driver     string `json:"Driver"`
	Name       string `json:"Name"`
	Mountpoint string `json:"Mountpoint"`
	CreatedAt  string `json:"CreatedAt"`
	Size       string `json:"Size"`
	InUse      bool   `json:"InUse"`
}

// DiskUsage is one row of rungv system df --format json.
type DiskUsage struct {
	Type        string `json:"Type"`
	TotalCount  int    `json:"TotalCount"`
	Active      int    `json:"Active"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
}

func decodeLines[T any](out []byte) ([]T, error) {
	var rows []T
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var row T
		if err := dec.Decode(&row); err != nil {
			if errors.Is(err, io.EOF) {
				return rows, nil
			}
			return rows, err
		}
		rows = append(rows, row)
	}
}

// Images lists the cached images.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	out, err := c.output(ctx, "images", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[Image](out)
}

// Volumes lists the local volumes.
func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	out, err := c.output(ctx, "volume", "ls", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[Volume](out)
}

// DiskUsage reports the totals of images and volumes.
func (c *Client) DiskUsage(ctx context.Context) ([]DiskUsage, error) {
	out, err := c.output(ctx, "system", "df", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[DiskUsage](out)
}

// InspectImage returns the indented JSON of rungv inspect.
func (c *Client) InspectImage(ctx context.Context, ref string) (string, error) {
	out, err := c.output(ctx, "inspect", ref)
	if err != nil {
		return "", err
	}
	var v any
	if json.Unmarshal(out, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty), nil
		}
	}
	return string(out), nil
}

// Pull downloads ref, handing progress lines to line.
func (c *Client) Pull(ctx context.Context, ref string, line func(string)) error {
	return c.Stream(ctx, line, "pull", ref)
}

// RemoveImage removes a tag or an image.
func (c *Client) RemoveImage(ctx context.Context, ref string, force bool) error {
	args := []string{"rmi"}
	if force {
		args = append(args, "-f")
	}
	_, err := c.output(ctx, append(args, ref)...)
	return err
}

// PruneImages deletes dangling images, or every image with all.
func (c *Client) PruneImages(ctx context.Context, all bool) (string, error) {
	args := []string{"image", "prune", "-f"}
	if all {
		args = append(args, "-a")
	}
	out, err := c.output(ctx, args...)
	return strings.TrimSpace(string(out)), err
}

// CreateVolume formats a named volume. driver is ext4lite (default) or hostfs.
func (c *Client) CreateVolume(ctx context.Context, name, driver string) error {
	args := []string{"volume", "create"}
	if driver != "" {
		args = append(args, "-d", driver)
	}
	if name != "" {
		args = append(args, name)
	}
	_, err := c.output(ctx, args...)
	return err
}

// RemoveVolume deletes an unlocked volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.output(ctx, "volume", "rm", name)
	return err
}

// PruneVolumes deletes every unlocked volume.
func (c *Client) PruneVolumes(ctx context.Context) (string, error) {
	out, err := c.output(ctx, "volume", "prune", "-f")
	return strings.TrimSpace(string(out)), err
}

// Checkpoint asks the sandbox listening on socket to write its bundle.
func (c *Client) Checkpoint(ctx context.Context, socket, dir string, leaveRunning bool) error {
	args := []string{"checkpoint", "-control-socket", socket}
	if dir != "" {
		args = append(args, "-checkpoint-dir", dir)
	}
	if leaveRunning {
		args = append(args, "-leave-running")
	}
	_, err := c.output(ctx, args...)
	return err
}

// Login stores registry credentials. The password goes over stdin.
func (c *Client) Login(ctx context.Context, server, user, password string) error {
	args := []string{"login", "-u", user, "--password-stdin"}
	if server != "" {
		args = append(args, server)
	}
	cmd := c.Command(ctx, args...)
	cmd.Stdin = strings.NewReader(password)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return nil
}

// Diagnose turns a failure message of rungv into advice a person can act
// on, or "" when it knows none.
func Diagnose(msg string, exitCode int) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "address already in use"):
		return "A published host port is already used by another program. Pick a free port or stop the program holding it."
	case strings.Contains(m, "in use") && strings.Contains(m, "volume"):
		return "The volume is mounted by another running sandbox. A volume can be used by one sandbox at a time."
	case strings.Contains(m, "entitlement") || strings.Contains(m, "hypervisor") && runtime.GOOS == "darwin":
		return "rungv must be signed with the com.apple.security.hypervisor entitlement. Use Settings → Sign rungv."
	case strings.Contains(m, "windows hypervisor platform") || strings.Contains(m, "whpx"):
		return "Enable the \"Windows Hypervisor Platform\" optional feature, or allow the systrap fallback for trusted workloads."
	case strings.Contains(m, "memory limit"):
		return "The guest passed its memory limit and was stopped. Raise the memory limit in the sandbox settings."
	case strings.Contains(m, "execution engine stopped the guest"):
		return "The execution engine failed under the guest (a virus scanner may have ended a helper process)."
	case strings.Contains(m, "manifest unknown") || strings.Contains(m, "not found") && strings.Contains(m, "image"):
		return "The image reference was not found in its registry. Check the name and tag."
	case strings.Contains(m, "unauthorized") || strings.Contains(m, "401"):
		return "The registry refused the request. Sign in to the registry first."
	case strings.Contains(m, "no such host") || strings.Contains(m, "dial tcp"):
		return "The registry could not be reached. Check the network or the proxy settings."
	case exitCode == 137:
		return "The guest was killed (exit 137): it passed its memory limit or the engine stopped it."
	}
	return ""
}

// FormatArgs quotes args for display as one shell command line.
func FormatArgs(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(Quote(a))
	}
	return b.String()
}

// Quote quotes s for a POSIX shell when it needs it.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=,@%+~", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SplitArgs splits a command line into words, honouring single and double
// quotes and backslash escapes, as a POSIX shell does without expansion.
func SplitArgs(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return words, fmt.Errorf("unterminated %c quote", quote)
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
