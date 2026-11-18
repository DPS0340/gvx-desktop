package procstat

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// getProcessMemoryInfo is psapi's GetProcessMemoryInfo, which kernel32
// has carried as K32GetProcessMemoryInfo since Windows 7.
var getProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// read opens each process to read its times and its working set.
func read(pids []int) (map[int]Sample, error) {
	out := make(map[int]Sample, len(pids))
	for _, pid := range pids {
		if s, ok := readOne(pid); ok {
			out[pid] = s
		}
	}
	return out, nil
}

func readOne(pid int) (Sample, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Sample{}, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return Sample{}, false
	}
	// A FILETIME of a duration counts 100-nanosecond intervals.
	ticks := func(f windows.Filetime) time.Duration {
		return time.Duration(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100
	}
	s := Sample{At: time.Now(), CPU: ticks(kernel) + ticks(user)}
	var mem processMemoryCounters
	mem.cb = uint32(unsafe.Sizeof(mem))
	if getProcessMemoryInfo.Find() != nil {
		return s, true // Call would panic
	}
	if ok, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mem)), uintptr(mem.cb)); ok != 0 {
		s.Memory = uint64(mem.workingSetSize)
	}
	return s, true
}
