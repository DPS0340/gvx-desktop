//go:build unix && !darwin && !linux

package term

// processes cannot be listed here, so only the leader is ended.
func processes() []int { return nil }
