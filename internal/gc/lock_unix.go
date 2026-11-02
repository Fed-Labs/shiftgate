//go:build linux

package gc

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func acquireStateLock(stateDir string) (*StateLock, error) {
	if stateDir == "" || filepath.Clean(stateDir) == "/" {
		return nil, errors.New("invalid state directory")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(stateDir, ".shift-state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("state directory is in use by the running agent")
	}
	return &StateLock{file: file}, nil
}
