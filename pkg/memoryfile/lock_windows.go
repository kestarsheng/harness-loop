//go:build windows

package memoryfile

import (
	"fmt"
	"os"
	"time"
)

const lockStaleAfter = 30 * time.Second

type fileLock struct {
	path string
	file *os.File
}

func acquireLock(path string, timeout time.Duration) (*fileLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			return &fileLock{path: path, file: f}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("memoryfile: open lock %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("memoryfile: acquire lock %s: %w", path, ErrLockTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *fileLock) release() error {
	if l == nil {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if l.path != "" {
		err := os.Remove(l.path)
		l.path = ""
		return err
	}
	return nil
}
