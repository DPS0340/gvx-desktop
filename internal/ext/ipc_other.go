//go:build windows

package ext

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
)

func listenPrivateUnixSocket(sockPath string) (net.Listener, error) {
	return nil, errors.New("unix IPC sockets are not used on windows")
}

func removeSocketIfSocket(sockPath string) {
	if sockPath == "" {
		return
	}
	if fi, err := os.Lstat(sockPath); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		_ = os.Remove(sockPath)
	}
}

func setProcessGroup(_ *exec.Cmd) {}

func signalTerminate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func signalForceKill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func isFileExecutable(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular()
}

func randomHexToken(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
