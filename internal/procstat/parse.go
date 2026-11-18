package procstat

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// userHZ is the clock tick /proc counts CPU time in: USER_HZ, 100 on
// every architecture Linux runs gvx on.
const userHZ = 100

// parseStat reads the CPU time and resident memory from a /proc/<pid>/stat
// line, whose second field, the command's name in parentheses, may hold
// spaces and parentheses of its own: the fields go on after its last ")".
// Counted from 1, utime is field 14, stime 15 and rss, in pages, 24.
func parseStat(line string, pageSize uint64) (Sample, error) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return Sample{}, fmt.Errorf("procstat: no command in %q", line)
	}
	f := strings.Fields(line[end+1:]) // f[0] is field 3, the state
	if len(f) < 22 {
		return Sample{}, fmt.Errorf("procstat: %d fields after the command", len(f))
	}
	utime, err1 := strconv.ParseUint(f[11], 10, 64)
	stime, err2 := strconv.ParseUint(f[12], 10, 64)
	rss, err3 := strconv.ParseInt(f[21], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return Sample{}, fmt.Errorf("procstat: fields %q %q %q", f[11], f[12], f[21])
	}
	ticks := utime + stime
	return Sample{
		CPU:    time.Duration(ticks) * (time.Second / userHZ),
		Memory: uint64(max(rss, 0)) * pageSize,
	}, nil
}

// parsePS reads what ps -o pid=,rss=,time= prints: a line a process, its
// pid, its resident memory in KiB and its CPU time.
func parsePS(out string, at time.Time) map[int]Sample {
	samples := map[int]Sample{}
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		rss, err2 := strconv.ParseUint(f[1], 10, 64)
		cpu, err3 := parseCPUTime(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		samples[pid] = Sample{At: at, CPU: cpu, Memory: rss << 10}
	}
	return samples
}

// parseCPUTime reads a CPU time as ps writes it, [[dd-]hh:]mm:ss[.ss]:
// macOS writes minutes and hundredths, 4:50.54, and others hours and
// whole seconds, 00:04:50.
func parseCPUTime(s string) (time.Duration, error) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("procstat: cpu time %q", s)
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("procstat: cpu time %q", s)
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || sec < 0 {
		return 0, fmt.Errorf("procstat: cpu time %q", s)
	}
	total := time.Duration(days)*24*time.Hour + time.Duration(sec*float64(time.Second))
	for i, unit := range []time.Duration{time.Minute, time.Hour} {
		at := len(parts) - 2 - i
		if at < 0 {
			break
		}
		n, err := strconv.Atoi(parts[at])
		if err != nil || n < 0 {
			return 0, fmt.Errorf("procstat: cpu time %q", s)
		}
		total += time.Duration(n) * unit
	}
	return total, nil
}
