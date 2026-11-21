package recipes

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// A path in docker run -v or in a compose file's bind mount names a path
// where dockerd runs, which is the sandbox, and compose writes a relative
// one as the absolute host path of the project. The Docker Engine recipe
// shares folders of this computer with the sandbox at the same paths, as
// Docker Desktop does, so that those paths name the same files there.
// Windows paths cannot be guest paths: C:\Users\me is shared at
// /c/Users/me, the form compose writes with COMPOSE_CONVERT_WINDOWS_PATHS=1.

// DefaultSharedFolders are the folders a new engine shares: the user's
// home folder.
func DefaultSharedFolders() []string { return []string{rungv.ExpandHome("~")} }

// ErrNoGuestPath reports a host path no guest path stands for.
var ErrNoGuestPath = errors.New("the path has no guest path")

// GuestPath is the path in the sandbox a shared host folder is at: the
// same path, or on Windows the drive as a folder, /c/Users/me.
func GuestPath(host string) (string, error) { return guestPath(runtime.GOOS, host) }

// WindowsGuestPath is the guest path of a Windows path, C:\Users\me or
// C:/Users/me, whatever system this is: what docker on Windows names a
// folder by is at /c/Users/me in the engine.
func WindowsGuestPath(host string) (string, error) { return guestPath("windows", host) }

func guestPath(goos, host string) (string, error) {
	if goos != "windows" {
		if !strings.HasPrefix(host, "/") {
			return "", ErrNoGuestPath
		}
		return path.Clean(host), nil
	}
	p := strings.ReplaceAll(host, `\`, "/")
	if len(p) < 3 || p[1] != ':' || p[2] != '/' || !(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') {
		return "", ErrNoGuestPath // relative, a UNC share, or a drive alone
	}
	return path.Clean("/" + strings.ToLower(p[:1]) + p[2:]), nil
}

// reservedGuestPaths are where the sandbox's own system is: a folder
// shared at one of them, or above one, would hide what dockerd runs on.
var reservedGuestPaths = []string{
	"/bin", "/sbin", "/lib", "/lib64", "/usr", "/etc", "/proc", "/sys", "/dev",
	"/run", "/var/run", "/var/lib/docker", "/root", "/tmp",
}

// under reports whether p is dir or inside it.
func under(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// ReservedGuestPath reports whether a folder cannot be shared at the guest
// path g: g is the sandbox's system's, inside it, or above it.
func ReservedGuestPath(g string) bool {
	for _, r := range reservedGuestPaths {
		if under(g, r) || under(r, g) {
			return true
		}
	}
	return false
}

// isShared reports whether a mount of the engine's sandbox shares a folder:
// a bind mount, writable, at the guest path of its source.
func isShared(m sandbox.Mount) bool {
	if m.Type != sandbox.MountBind || m.ReadOnly {
		return false
	}
	g, err := GuestPath(rungv.ExpandHome(m.Source))
	return err == nil && g == m.Target
}

// SharedFoldersOf reads from the engine's sandbox the host folders it
// shares, which is what counts once the sandbox exists: its mounts are its
// own.
func SharedFoldersOf(spec sandbox.Spec) []string {
	var out []string
	for _, m := range spec.Mounts {
		if isShared(m) {
			out = append(out, rungv.ExpandHome(m.Source))
		}
	}
	return out
}

// SharedGuestPaths are the guest paths the engine's sandbox has shared
// folders at.
func SharedGuestPaths(spec sandbox.Spec) []string {
	var out []string
	for _, m := range spec.Mounts {
		if isShared(m) {
			out = append(out, m.Target)
		}
	}
	return out
}

// WithSharedFolders returns the engine's sandbox sharing folders, its other
// mounts untouched. A folder with no guest path is left out. The change
// takes a restart: a sandbox's mounts are fixed when it starts.
func WithSharedFolders(spec sandbox.Spec, folders []string) sandbox.Spec {
	spec = spec.Clone()
	spec.Mounts = slices.DeleteFunc(spec.Mounts, isShared)
	for _, f := range folders {
		if g, err := GuestPath(f); err == nil {
			spec.Mounts = append(spec.Mounts, sandbox.Mount{Type: sandbox.MountBind, Source: f, Target: g})
		}
	}
	return spec
}

// Unshared reports whether a container's bind mount source, a path in the
// sandbox, looks like a folder of this computer that is not shared: it is
// outside every shared folder and outside the sandbox's own system, where a
// container may well mean the sandbox's file, as /var/run/docker.sock.
// dockerd made such a source an empty folder in the sandbox.
func Unshared(source string, shared []string) bool {
	if !strings.HasPrefix(source, "/") {
		return false
	}
	source = path.Clean(source)
	for _, s := range shared {
		if under(source, s) {
			return false
		}
	}
	for _, r := range reservedGuestPaths {
		if under(source, r) {
			return false
		}
	}
	return true
}

// UnsharedBind reports whether source, a container bind mount source path,
// looks like a host path outside the folders engine shares. It returns false
// when engine is not a Docker Engine sandbox, when source is not an absolute
// path (such as a named volume), or when source is inside a shared folder or
// the sandbox's own system (such as /var/run/docker.sock).
func UnsharedBind(source string, engine sandbox.Spec) bool {
	if engine.Recipe != Docker {
		return false
	}
	return Unshared(source, SharedGuestPaths(engine))
}

// Why a folder cannot be shared.
var (
	ErrNotFolder     = errors.New("not a folder")
	ErrReserved      = errors.New("the sandbox's system is at that path")
	ErrAlreadyShared = errors.New("already shared")
)

// ShareError is a folder that cannot be shared, and why: Err is one of the
// errors above, and Folder the shared folder it is inside, for
// ErrAlreadyShared.
type ShareError struct {
	Err    error
	Folder string
}

func (e *ShareError) Error() string { return e.Err.Error() }
func (e *ShareError) Unwrap() error { return e.Err }

// AddSharedFolder adds the host folder dir to those shared, or says why it
// cannot be: it must be an absolute path of a folder that exists, have a
// guest path that is not the sandbox's system's, and not be inside a
// folder shared already. Shared folders inside it give way to it.
func AddSharedFolder(folders []string, dir string) ([]string, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if !filepath.IsAbs(dir) {
		return folders, &ShareError{Err: ErrNoGuestPath}
	}
	g, err := GuestPath(dir)
	if err != nil {
		return folders, &ShareError{Err: ErrNoGuestPath}
	}
	if ReservedGuestPath(g) {
		return folders, &ShareError{Err: ErrReserved}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return folders, &ShareError{Err: ErrNotFolder}
	}
	var out []string
	for _, f := range folders {
		fg, err := GuestPath(f)
		switch {
		case err != nil:
			out = append(out, f)
		case under(g, fg):
			return folders, &ShareError{Err: ErrAlreadyShared, Folder: f}
		case !under(fg, g):
			out = append(out, f) // one inside dir is shared through it now
		}
	}
	return append(out, dir), nil
}
