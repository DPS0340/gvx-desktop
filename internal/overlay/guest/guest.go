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
	"sync"
)

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

// Install writes bin, netinit, to dir/netinit for sandboxes to mount, unless
// it is there already, and returns its path.
func Install(dir string, bin []byte) (string, error) {
	p := filepath.Join(dir, "netinit")
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, bin) {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".netinit-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(bin)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o755), os.Rename(tmp.Name(), p)); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return p, nil
}
