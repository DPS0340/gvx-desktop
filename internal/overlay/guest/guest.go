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
