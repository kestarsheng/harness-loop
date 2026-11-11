//go:build !windows

package memoryfile

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

type fileLock struct {
	file *os.File
}

func acquireLock(path string, timeout time.Duration) (*fileLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("memoryfile: open lock %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return &fileLock{file: f}, nil
		}
		_ = f.Close()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("memoryfile: acquire lock %s: %w", path, ErrLockTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *fileLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
