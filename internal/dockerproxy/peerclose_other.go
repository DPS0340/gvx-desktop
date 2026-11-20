//go:build !darwin && !linux

package dockerproxy

import "net"

// waitPeerClose waits for done on platforms without Unix socket peer-close
// edge detection.
func waitPeerClose(_ net.Conn, done <-chan struct{}) bool {
	<-done
	return false
}
