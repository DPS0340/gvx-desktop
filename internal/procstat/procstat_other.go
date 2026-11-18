//go:build !unix && !windows

package procstat

func read([]int) (map[int]Sample, error) { return nil, ErrUnsupported }
