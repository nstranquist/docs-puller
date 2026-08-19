//go:build !windows

package main

import (
	"os"
	"syscall"
)

func lockFile(file *os.File) (unlock func() error, err error) {
	// LOCK_EX without LOCK_NB blocks until the lock is available.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() error {
		return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}, nil
}
