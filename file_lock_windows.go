//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(file *os.File) (unlock func() error, err error) {
	overlapped := &windows.Overlapped{}
	const wholeFile = ^uint32(0)
	handle := windows.Handle(file.Fd())
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, wholeFile, wholeFile, overlapped); err != nil {
		return nil, err
	}
	return func() error {
		return windows.UnlockFileEx(handle, 0, wholeFile, wholeFile, overlapped)
	}, nil
}
