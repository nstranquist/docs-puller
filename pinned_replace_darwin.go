//go:build darwin

package main

import "golang.org/x/sys/unix"

func swapPinnedSourceDirs(stageDir, sourceDir string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, stageDir, unix.AT_FDCWD, sourceDir, unix.RENAME_SWAP)
}
