//go:build !darwin

package main

func swapPinnedSourceDirs(stageDir, sourceDir string) error {
	return errAtomicDirSwapUnsupported
}
