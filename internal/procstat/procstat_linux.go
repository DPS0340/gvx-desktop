package procstat

import (
	"os"
	"strconv"
	"time"
)

// read reads each process's /proc/<pid>/stat.
func read(pids []int) (map[int]Sample, error) {
	out := make(map[int]Sample, len(pids))
	page := uint64(os.Getpagesize())
	for _, pid := range pids {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			continue
		}
		if s, err := parseStat(string(b), page); err == nil {
			s.At = time.Now()
			out[pid] = s
		}
	}
	return out, nil
}
