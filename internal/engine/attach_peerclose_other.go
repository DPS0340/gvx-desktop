//go:build !darwin && !linux

package engine

// Owner: workstream A.

import "net"

// attachWaitPeerClose waits for done on platforms without socket peer-close
// edge detection.
func attachWaitPeerClose(_ net.Conn, done <-chan struct{}) bool {
	<-done
	return false
}
