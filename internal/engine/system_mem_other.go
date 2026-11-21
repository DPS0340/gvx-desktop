//go:build !darwin && !linux

package engine

// Owner: workstream C.

func hostMemTotal() int64 { return 0 }
