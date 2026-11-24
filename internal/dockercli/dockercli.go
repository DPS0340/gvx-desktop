// Package dockercli locates and exposes the official Docker CLI binaries
// (docker, docker-buildx, docker-compose) bundled inside GVX Desktop release
// packages, and optionally links ~/.gvx/bin into the user's terminal PATH.
package dockercli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Bundle holds the paths to the bundled Docker CLI binaries shipped with the
// application, when present.
type Bundle struct {
	Docker     string // bundled docker binary
	Buildx     string // bundled docker-buildx plugin
	Compose    string // bundled docker-compose plugin
	PluginsDir string // directory containing bundled cli-plugins
}

// Available reports whether a bundled docker CLI binary was found.
func (b Bundle) Available() bool { return b.Docker != "" }

// HasPlugins reports whether at least one bundled CLI plugin was found.
func (b Bundle) HasPlugins() bool { return b.Buildx != "" || b.Compose != "" }

// Hooks replaced in tests so unit tests never touch the host's home or real
// executable directory.
var (
	executablePath = os.Executable
	userHomeDir    = os.UserHomeDir
	goos           = func() string { return runtime.GOOS }
	goarch         = func() string { return runtime.GOARCH }
	getenv         = os.Getenv
)

func exeName(base, targetOS string) string {
	if targetOS == "windows" {
		return base + ".exe"
	}
	return base
}

// Bundled locates the Docker CLI binaries bundled with this copy of GVX Desktop.
func Bundled() Bundle {
	targetOS := goos()
	if env := strings.TrimSpace(getenv("GVX_DOCKER_CLI_DIR")); env != "" {
		if strings.EqualFold(env, "none") {
			return Bundle{}
		}
		if b, ok := probeRoot(env, targetOS); ok {
			return b
		}
		return Bundle{}
	}

	var roots []string
	addExeRoots := func(exe string) {
		if exe == "" {
			return
		}
		dir := filepath.Dir(exe)
		switch targetOS {
		case "darwin":
			roots = append(roots,
				filepath.Join(dir, "..", "Helpers"),
				filepath.Join(dir, "..", "Resources"),
			)
		case "linux":
			roots = append(roots,
				filepath.Join(dir, "..", "lib", "gvx-desktop"),
			)
		}
		roots = append(roots, dir, filepath.Join(dir, "docker-cli"))
	}
	if exe, err := executablePath(); err == nil {
		addExeRoots(exe)
		if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
			addExeRoots(resolved)
		}
	}
	// Development checkouts can fetch bundled binaries into
	// build/docker-cli/<os>_<arch> to exercise bundled CLI mode locally.
	// Skip this during `go test` so unit tests stay isolated from developer
	// artifacts on disk.
	if !testing.Testing() {
		if wd, err := os.Getwd(); err == nil {
			roots = append(roots, filepath.Join(wd, "build", "docker-cli", targetOS+"_"+goarch()))
		}
	}

	seen := make(map[string]bool, len(roots))
	for _, r := range roots {
		clean := filepath.Clean(r)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if b, ok := probeRoot(clean, targetOS); ok {
			return b
		}
	}
	return Bundle{}
}

func probeRoot(root, targetOS string) (Bundle, bool) {
	dockerFile := exeName("docker", targetOS)
	var dockerPath string
	for _, cand := range []string{
		filepath.Join(root, "bin", dockerFile),
		filepath.Join(root, dockerFile),
	} {
		if isExecutableFile(cand, targetOS) {
			dockerPath = cand
			break
		}
	}
	if dockerPath == "" {
		return Bundle{}, false
	}
	b := Bundle{Docker: dockerPath}
	pluginsDir := filepath.Join(root, "cli-plugins")
	buildx := filepath.Join(pluginsDir, exeName("docker-buildx", targetOS))
	compose := filepath.Join(pluginsDir, exeName("docker-compose", targetOS))
	if isExecutableFile(buildx, targetOS) {
		b.Buildx = buildx
	}
	if isExecutableFile(compose, targetOS) {
		b.Compose = compose
	}
	if b.HasPlugins() {
		b.PluginsDir = pluginsDir
	}
	return b, true
}

func isExecutableFile(p, targetOS string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if targetOS != "windows" && st.Mode()&0o111 == 0 {
		return false
	}
	return true
}

// BinDir returns ~/.gvx/bin, where GVX Desktop keeps user-facing CLI binaries
// (rungv and the bundled docker symlink/copy).
func BinDir() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gvx", "bin"), nil
}

// ManagedPluginsDir returns ~/.gvx/cli-plugins, where GVX Desktop exposes the
// bundled docker-buildx and docker-compose plugins for the docker CLI.
func ManagedPluginsDir() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gvx", "cli-plugins"), nil
}

// ManagedDockerPath returns ~/.gvx/bin/docker (or docker.exe on Windows).
func ManagedDockerPath() (string, error) {
	return ManagedBinaryPath("docker")
}

// ManagedBinaryPath returns ~/.gvx/bin/<baseName> (or <baseName>.exe on Windows).
func ManagedBinaryPath(baseName string) (string, error) {
	dir, err := BinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, exeName(baseName, goos())), nil
}

// BinaryLinkedInBinDir reports whether src is already located at or synced
// into ~/.gvx/bin/<baseName>[.exe].
func BinaryLinkedInBinDir(src, baseName string) bool {
	if strings.TrimSpace(src) == "" || strings.TrimSpace(baseName) == "" {
		return false
	}
	dst, err := ManagedBinaryPath(baseName)
	if err != nil {
		return false
	}
	if sameFileOnDisk(src, dst) {
		return true
	}
	if goos() == "windows" {
		srcSum, err := fileSHA256(src)
		if err != nil {
			return false
		}
		prev, err := os.ReadFile(dst + ".gvx-sha256")
		if err != nil || strings.TrimSpace(string(prev)) != srcSum {
			return false
		}
		dstSum, err := fileSHA256(dst)
		return err == nil && dstSum == srcSum
	}
	return false
}

// SyncBinaryToBinDir exposes src at ~/.gvx/bin/<baseName>[.exe] (symlink on
// Unix, hash-tracked copy on Windows). If src is already at that path, it is
// a no-op. If a different unmanaged regular file already occupies the slot, it
// returns an error instead of overwriting it.
func SyncBinaryToBinDir(src, baseName string) (string, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return "", errors.New("binary path is empty")
	}
	targetOS := goos()
	if !isExecutableFile(src, targetOS) {
		return "", fmt.Errorf("%s is not an executable file", src)
	}
	dst, err := ManagedBinaryPath(baseName)
	if err != nil {
		return "", err
	}
	if sameFileOnDisk(src, dst) {
		return dst, nil
	}
	if targetOS != "windows" {
		if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s already exists and points to a different binary", dst)
		}
	} else {
		if _, err := os.Stat(dst); err == nil {
			if _, merr := os.Stat(dst + ".gvx-sha256"); errors.Is(merr, os.ErrNotExist) {
				return "", fmt.Errorf("%s already exists and is not managed by GVX Desktop", dst)
			}
		}
	}
	if err := syncManagedBinary(src, dst, targetOS); err != nil {
		return "", err
	}
	return dst, nil
}

func sameFileOnDisk(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	if samePath(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	if errA == nil && errB == nil && os.SameFile(sa, sb) {
		return true
	}
	return false
}

// DockerConfigFile returns the path to Docker's config.json, honoring
// $DOCKER_CONFIG when set.
func DockerConfigFile() (string, error) {
	if dir := strings.TrimSpace(getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// IsBundledPath reports whether p refers to the bundled docker CLI binary
// (either directly inside the app bundle or via ~/.gvx/bin/docker).
func IsBundledPath(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	b := Bundled()
	if !b.Available() {
		return false
	}
	norm := func(path string) string {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			path = r
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		return filepath.Clean(path)
	}
	np := norm(p)
	if samePath(np, norm(b.Docker)) {
		return true
	}
	if managed, err := ManagedDockerPath(); err == nil {
		if samePath(filepath.Clean(p), filepath.Clean(managed)) || samePath(np, norm(managed)) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if goos() == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SyncResult describes what Sync linked on disk.
type SyncResult struct {
	Bundle     Bundle
	DockerPath string // ~/.gvx/bin/docker (or direct b.Docker if linking skipped)
	PluginsDir string // ~/.gvx/cli-plugins
}

// BundleAt inspects root for bundled Docker CLI binaries (bin/docker or docker,
// plus optional cli-plugins/docker-buildx and cli-plugins/docker-compose).
func BundleAt(root string) Bundle {
	if strings.TrimSpace(root) == "" {
		return Bundle{}
	}
	if b, ok := probeRoot(filepath.Clean(root), goos()); ok {
		return b
	}
	return Bundle{}
}

// Sync exposes the bundled Docker CLI in ~/.gvx/bin and its plugins in
// ~/.gvx/cli-plugins. When registerPlugins is true and bundled plugins are
// present, it also adds ~/.gvx/cli-plugins to cliPluginsExtraDirs in Docker's
// config.json so docker buildx and docker compose work in every terminal.
func Sync(registerPlugins bool) (SyncResult, error) {
	return SyncBundle(Bundled(), registerPlugins)
}

// SyncFromDir locates bundled Docker CLI binaries under root and syncs them
// into ~/.gvx/bin and ~/.gvx/cli-plugins. Useful for installers that just
// extracted the application files into a target directory.
func SyncFromDir(root string, registerPlugins bool) (SyncResult, error) {
	return SyncBundle(BundleAt(root), registerPlugins)
}

// SyncBundle exposes b in ~/.gvx/bin and ~/.gvx/cli-plugins.
func SyncBundle(b Bundle, registerPlugins bool) (SyncResult, error) {
	if !b.Available() {
		return SyncResult{}, nil
	}
	res := SyncResult{Bundle: b, DockerPath: b.Docker}
	binDir, err := BinDir()
	if err != nil {
		return res, err
	}
	targetOS := goos()
	managedDocker := filepath.Join(binDir, exeName("docker", targetOS))
	if err := syncManagedBinary(b.Docker, managedDocker, targetOS); err != nil {
		return res, err
	}
	if isExecutableFile(managedDocker, targetOS) {
		res.DockerPath = managedDocker
	}

	if b.HasPlugins() {
		plugDir, err := ManagedPluginsDir()
		if err != nil {
			return res, err
		}
		if b.Buildx != "" {
			if err := syncManagedBinary(b.Buildx, filepath.Join(plugDir, exeName("docker-buildx", targetOS)), targetOS); err != nil {
				return res, err
			}
		}
		if b.Compose != "" {
			if err := syncManagedBinary(b.Compose, filepath.Join(plugDir, exeName("docker-compose", targetOS)), targetOS); err != nil {
				return res, err
			}
		}
		res.PluginsDir = plugDir
		if registerPlugins {
			if err := EnsurePluginsConfigured(plugDir); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// syncManagedBinary makes dst point to src inside a GVX-managed directory
// (~/.gvx/bin or ~/.gvx/cli-plugins). On Unix it writes an atomic symlink
// without overwriting a regular file placed there by the user. On Windows it
// copies the binary and tracks a sidecar hash file so upgrades refresh it
// safely without needing symlink privileges.
func syncManagedBinary(src, dst, targetOS string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if targetOS != "windows" {
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&os.ModeSymlink == 0 {
				// Respect a non-symlink binary the user put there themselves.
				return nil
			}
			if cur, err := os.Readlink(dst); err == nil && cur == src {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		tmp := dst + ".gvx-tmp"
		_ = os.Remove(tmp)
		if err := os.Symlink(src, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}

	// Windows: copy without requiring Developer Mode / admin symlink privileges.
	srcSum, err := fileSHA256(src)
	if err != nil {
		return err
	}
	marker := dst + ".gvx-sha256"
	if _, err := os.Stat(dst); err == nil {
		prev, merr := os.ReadFile(marker)
		if merr != nil {
			// No marker: a user-placed binary is left untouched.
			return nil
		}
		if strings.TrimSpace(string(prev)) == srcSum {
			if dstSum, derr := fileSHA256(dst); derr == nil && dstSum == srcSum {
				return nil
			}
		}
	}
	tmp := dst + ".gvx-tmp"
	if err := copyFile(src, tmp, 0o755); err != nil {
		return err
	}
	old := dst + ".old"
	_ = os.Remove(old)
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(old, dst)
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(old)
	return os.WriteFile(marker, []byte(srcSum+"\n"), 0o644)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, mode os.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// EnsurePluginsConfigured adds pluginsDir to cliPluginsExtraDirs in Docker's
// config.json while preserving every other key in the file.
func EnsurePluginsConfigured(pluginsDir string) error {
	if strings.TrimSpace(pluginsDir) == "" {
		return nil
	}
	cfgPath, err := DockerConfigFile()
	if err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(cfgPath); err == nil {
		trimmed := strings.TrimSpace(string(b))
		if trimmed != "" {
			if err := json.Unmarshal(b, &raw); err != nil {
				return fmt.Errorf("reading %s: %w", cfgPath, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var dirs []string
	if existing, ok := raw["cliPluginsExtraDirs"]; ok && len(existing) > 0 && string(existing) != "null" {
		if err := json.Unmarshal(existing, &dirs); err != nil {
			return fmt.Errorf("reading cliPluginsExtraDirs in %s: %w", cfgPath, err)
		}
	}
	cleanTarget := filepath.Clean(pluginsDir)
	for _, d := range dirs {
		if samePath(filepath.Clean(d), cleanTarget) {
			return nil
		}
	}
	dirs = append(dirs, cleanTarget)
	encoded, err := json.Marshal(dirs)
	if err != nil {
		return err
	}
	raw["cliPluginsExtraDirs"] = encoded

	out, err := json.MarshalIndent(raw, "", "\t")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(cfgPath), ".config-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// SystemHasPlugin reports whether a Docker CLI plugin named name (for example
// "docker-buildx") is already installed outside GVX Desktop's managed plugins
// directory.
func SystemHasPlugin(name string) bool {
	targetOS := goos()
	file := exeName(name, targetOS)
	var dirs []string
	if dir := strings.TrimSpace(getenv("DOCKER_CONFIG")); dir != "" {
		dirs = append(dirs, filepath.Join(dir, "cli-plugins"))
	} else if home, err := userHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".docker", "cli-plugins"))
	}
	if targetOS == "windows" {
		for _, v := range []string{"ProgramData", "ProgramFiles", "ProgramW6432"} {
			if base := getenv(v); base != "" {
				dirs = append(dirs, filepath.Join(base, "Docker", "cli-plugins"))
			}
		}
	} else {
		dirs = append(dirs,
			"/usr/local/lib/docker/cli-plugins",
			"/usr/local/libexec/docker/cli-plugins",
			"/usr/lib/docker/cli-plugins",
			"/usr/libexec/docker/cli-plugins",
			"/opt/homebrew/lib/docker/cli-plugins",
		)
	}
	return slices.ContainsFunc(dirs, func(d string) bool {
		return isExecutableFile(filepath.Join(d, file), targetOS)
	})
}
