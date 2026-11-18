// Package procstat samples what a process of this computer uses: the CPU
// time it has had and its resident memory. A gvx sandbox is one rungv
// process, guest and all, so its sample is the sandbox's.
package procstat

import (
	"errors"
	"time"
)

// Sample is what a process had used when it was read.
type Sample struct {
	At     time.Time
	CPU    time.Duration // user and system time, since it started
	Memory uint64        // resident memory, in bytes
}

// ErrUnsupported is what Read answers on a system it cannot read.
var ErrUnsupported = errors.New("procstat: not supported on this system")

// Read samples the processes of pids. A process that is gone, or that
// cannot be read, is missing from the map; err is only for a reading that
// failed as a whole.
func Read(pids []int) (map[int]Sample, error) {
	if len(pids) == 0 {
		return map[int]Sample{}, nil
	}
	return read(pids)
}

// CPUPercent is the share of a CPU a process used between two samples:
// 100 for one CPU kept busy, more for more, as docker stats counts it. ok
// is false when prev is no earlier sample of the same run.
func CPUPercent(prev, cur Sample) (percent float64, ok bool) {
	wall := cur.At.Sub(prev.At)
	if prev.At.IsZero() || wall <= 0 || cur.CPU < prev.CPU {
		return 0, false
	}
	return float64(cur.CPU-prev.CPU) / float64(wall) * 100, true
}
