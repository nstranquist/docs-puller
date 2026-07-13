//go:build !darwin

package main

import "errors"

var errAtomicDirSwapUnsupported = errors.New("atomic directory swap unsupported")

func swapPinnedSourceDirs(stageDir, sourceDir string) error {
	return errAtomicDirSwapUnsupported
}
