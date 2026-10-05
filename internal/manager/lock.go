package manager

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type updateLock struct{ file *os.File }

func acquireUpdateLock(dir string) (*updateLock, error) {
	stateDir := filepath.Join(dir, ".update-state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(stateDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errUpdateBusy
		}
		return nil, err
	}
	return &updateLock{file: f}, nil
}
func (l *updateLock) Close() {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}
