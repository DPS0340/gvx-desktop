//go:build !windows

package ext

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const staleDialTimeout = 500 * time.Millisecond

func listenPrivateUnixSocket(sockPath string) (net.Listener, error) {
	if len(sockPath) > MaxUnixSocketPathLen {
		return nil, fmt.Errorf("unix socket path %q exceeds %d-byte sun_path limit", sockPath, MaxUnixSocketPathLen)
	}
	dir := filepath.Dir(sockPath)
	if err := ensurePrivateSocketDir(dir); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s already exists and is not a socket", sockPath)
		}
		c, derr := net.DialTimeout("unix", sockPath, staleDialTimeout)
		if derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another process is already listening on %s", sockPath)
		}
		if !errors.Is(derr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot check existing socket %s: %w", sockPath, derr)
		}
		_ = os.Remove(sockPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp(dir, ".s*")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	tmpSock := filepath.Join(tmpDir, "s")
	ln, err := net.Listen("unix", tmpSock)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(tmpSock, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	if err := os.Rename(tmpSock, sockPath); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	_ = os.Remove(tmpDir)
	return ln, nil
}

func removeSocketIfSocket(sockPath string) {
	if sockPath == "" {
		return
	}
	if fi, err := os.Lstat(sockPath); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		_ = os.Remove(sockPath)
	}
}

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalTerminate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
}

func signalForceKill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

func isFileExecutable(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func randomHexToken(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
