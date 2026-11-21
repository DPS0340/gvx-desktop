package engine

// Owner: frozen (architect). Small helpers more than one workstream needs.
// Ask the coordinator to add one here rather than writing a second copy:
// the package is one namespace.

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// randomHex64 is 64 random hex digits: a container's, network's or
// anonymous volume's ID.
func randomHex64() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// volumeNameRe is what a volume name may be, for docker and rungv alike.
var volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
