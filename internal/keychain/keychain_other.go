//go:build !darwin && !linux && !windows

package keychain

import "errors"

// The other systems have no store gvx-desktop reaches.
func system() (Keychain, error) {
	return nil, errors.New("gvx-desktop reaches no keychain on this system")
}
