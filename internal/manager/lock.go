package manager

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type updateLock struct {
	file      *os.File
	inherited bool
}

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
	if _, err = os.Stat(filepath.Join(stateDir, "deployment.pending")); !errors.Is(err, os.ErrNotExist) {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, errors.New("上次发布尚未恢复，请重新执行 mise run publish")
	}
	return &updateLock{file: f}, nil
}
func (l *updateLock) Close() {
	if !l.inherited {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	}
	_ = l.file.Close()
}

// flock ownership stays with the publisher's open-file description. Children
// close only their descriptor; unlocking it would release the parent's lock.
func inheritedUpdateLock(dir string, fd int) (*updateLock, error) {
	if fd < 3 {
		return nil, errors.New("无效的继承更新锁")
	}
	f := os.NewFile(uintptr(fd), "publisher-update-lock")
	if f == nil {
		return nil, errors.New("继承更新锁不存在")
	}
	actual, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	expected, err := os.Stat(filepath.Join(dir, ".update-state", "update.lock"))
	if err != nil || !os.SameFile(actual, expected) {
		f.Close()
		return nil, errors.New("继承更新锁与部署目录不匹配")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errUpdateBusy
	}
	return &updateLock{file: f, inherited: true}, nil
}
