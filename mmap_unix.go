//go:build !windows

package main

import (
	"os"
	"syscall"

	"github.com/nstranquist/docs-puller/searchruntime"
)

func mmapReadOnly(path string) ([]byte, func(), error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() == 0 {
		return nil, nil, searchruntime.EmbeddingFlatIndexVectorFileEmptyError(path)
	}
	data, err := syscall.Mmap(int(file.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() { _ = syscall.Munmap(data) }, nil
}
