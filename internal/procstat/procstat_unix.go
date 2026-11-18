//go:build unix && !linux

package procstat

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// read asks ps, once for every process: macOS keeps no /proc, and the
// call that reads a process's usage needs cgo there.
func read(pids []int) (map[int]Sample, error) {
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=,time=", "-p", strings.Join(list, ",")).Output()
	// ps fails when none of the processes is left, and prints nothing.
	if err != nil && len(out) == 0 {
		if _, exited := err.(*exec.ExitError); exited {
			return map[int]Sample{}, nil
		}
		return nil, err
	}
	return parsePS(string(out), time.Now()), nil
}
