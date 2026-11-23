//go:build windows

package dockercli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const windowsPathProfile = "Windows User PATH"

// PathStatus reports whether ~/.gvx/bin is already on the Windows user PATH.
func PathStatus() PathState {
	dir, err := BinDir()
	if err != nil {
		return PathState{}
	}
	st := PathState{
		Dir:     dir,
		Profile: windowsPathProfile,
		InEnv:   pathListContains(getenv("PATH"), dir, os.PathListSeparator),
	}
	if val, _, err := readUserPath(); err == nil {
		if containsWindowsPathEntry(val, dir) {
			st.InProfile = true
			st.Managed = true
		}
	}
	return st
}

// AddToPath appends ~/.gvx/bin to HKCU\Environment\Path and notifies shells.
func AddToPath() (PathState, error) {
	dir, err := BinDir()
	if err != nil {
		return PathState{}, err
	}
	val, valType, err := readUserPath()
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return PathStatus(), err
	}
	if containsWindowsPathEntry(val, dir) {
		return PathStatus(), nil
	}
	var updated string
	if strings.TrimSpace(val) == "" {
		updated = dir
	} else {
		updated = strings.TrimRight(val, ";") + ";" + dir
	}
	if err := writeUserPath(updated, valType); err != nil {
		return PathStatus(), err
	}
	broadcastEnvChange()
	return PathStatus(), nil
}

// RemoveFromPath removes ~/.gvx/bin from HKCU\Environment\Path.
func RemoveFromPath() (PathState, error) {
	dir, err := BinDir()
	if err != nil {
		return PathState{}, err
	}
	val, valType, err := readUserPath()
	if errors.Is(err, registry.ErrNotExist) {
		return PathStatus(), nil
	}
	if err != nil {
		return PathStatus(), err
	}
	cleanDir := filepath.Clean(dir)
	var kept []string
	for part := range strings.SplitSeq(val, ";") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		expanded := expandUserProfile(p)
		if strings.EqualFold(filepath.Clean(p), cleanDir) || strings.EqualFold(filepath.Clean(expanded), cleanDir) {
			continue
		}
		kept = append(kept, part)
	}
	if err := writeUserPath(strings.Join(kept, ";"), valType); err != nil {
		return PathStatus(), err
	}
	broadcastEnvChange()
	return PathStatus(), nil
}

func containsWindowsPathEntry(val, dir string) bool {
	cleanDir := filepath.Clean(dir)
	for part := range strings.SplitSeq(val, ";") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(p), cleanDir) || strings.EqualFold(filepath.Clean(expandUserProfile(p)), cleanDir) {
			return true
		}
	}
	return false
}

func expandUserProfile(p string) string {
	home, err := userHomeDir()
	if err != nil || home == "" {
		return p
	}
	lower := strings.ToLower(p)
	if strings.HasPrefix(lower, "%userprofile%") {
		return home + p[len("%userprofile%"):]
	}
	return p
}

func readUserPath() (string, uint32, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return "", registry.EXPAND_SZ, err
	}
	defer k.Close()
	val, valType, err := k.GetStringValue("Path")
	if err != nil {
		return "", registry.EXPAND_SZ, err
	}
	return val, valType, nil
}

func writeUserPath(val string, valType uint32) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if valType == registry.SZ {
		return k.SetStringValue("Path", val)
	}
	return k.SetExpandStringValue("Path", val)
}

func broadcastEnvChange() {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("SendMessageTimeoutW")
	env, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	const (
		hwndBroadcast = 0xffff
		wmSettingChg  = 0x001a
		smtoAbortHung = 0x0002
	)
	var result uintptr
	_, _, _ = proc.Call(
		hwndBroadcast,
		wmSettingChg,
		0,
		uintptr(unsafe.Pointer(env)),
		smtoAbortHung,
		5000,
		uintptr(unsafe.Pointer(&result)),
	)
}
