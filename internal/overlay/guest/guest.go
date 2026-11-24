// Package guest holds netinit, built for Linux on the host's architecture,
// for the engine to mount into containers on user networks. The binaries
// are generated: after changing netinit or guestcfg, run
//
//	go generate ./internal/overlay/guest
//
// and commit the result. A test fails while they are out of date.
package guest

//go:generate go run ./mkguest

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

const (
	// installDirName is the subdirectory created inside dir to hold netinit
	// and its hostfs .gvx_meta sidecar for read-only directory bind mounting.
	installDirName = "netinit.d"

	// hostfsMetaFile is rungv hostfs's workspace metadata sidecar filename.
	hostfsMetaFile = ".gvx_meta"

	// hostfsMetaMode is S_IFREG | 0o755 (0o100755 == 33261), the POSIX mode
	// recorded in .gvx_meta so Windows sandboxes (where NTFS has no execute
	// bit) expose netinit as an executable regular file.
	hostfsMetaJSON = `{"version":2,"entries":{"` + guestcfg.BinaryName + `":{"mode":33261,"uid":0,"gid":0}}}` + "\n"
)

var hostfsMetaBytes = []byte(hostfsMetaJSON)

// ErrUnavailable reports that this build of the app has no netinit for its
// architecture.
var ErrUnavailable = errors.New("netinit is not built for this architecture")

var (
	once     sync.Once
	binary   []byte
	extractE error
)

// Netinit returns the netinit executable for linux on the host's
// architecture.
func Netinit() ([]byte, error) {
	once.Do(func() {
		if len(compressed) == 0 {
			extractE = ErrUnavailable
			return
		}
		r, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			extractE = err
			return
		}
		binary, extractE = io.ReadAll(r)
	})
	return binary, extractE
}

// Install writes bin (netinit) and a hostfs .gvx_meta sidecar into
// dir/netinit.d for sandboxes to bind-mount read-only at guestcfg.MountDir,
// unless they are already up to date, and returns the directory path.
func Install(dir string, bin []byte) (string, error) {
	mountDir := filepath.Join(dir, installDirName)
	binPath := filepath.Join(mountDir, guestcfg.BinaryName)
	metaPath := filepath.Join(mountDir, hostfsMetaFile)

	binOK := fileMatches(binPath, bin, true)
	metaOK := fileMatches(metaPath, hostfsMetaBytes, false)
	if binOK && metaOK {
		return mountDir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(mountDir, 0o755); err != nil {
		return "", err
	}
	if !binOK {
		if err := writeFileAtomic(mountDir, guestcfg.BinaryName, ".netinit-*", bin, 0o755); err != nil {
			return "", err
		}
	}
	if !metaOK {
		if err := writeFileAtomic(mountDir, hostfsMetaFile, ".gvx_meta.tmp.*", hostfsMetaBytes, 0o644); err != nil {
			return "", err
		}
	}
	return mountDir, nil
}

func fileMatches(path string, want []byte, wantExec bool) bool {
	cur, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(cur, want) {
		return false
	}
	if wantExec && runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm()&0o111 != 0o111 {
			return false
		}
	}
	return true
}

func writeFileAtomic(dir, name, tempPattern string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmpName, perm), os.Rename(tmpName, filepath.Join(dir, name))); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
