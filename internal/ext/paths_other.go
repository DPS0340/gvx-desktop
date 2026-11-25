//go:build windows

package ext

func currentEUID() int { return -1 }

func ensurePrivateSocketDir(dir string) error {
	return EnsureDir(dir)
}
