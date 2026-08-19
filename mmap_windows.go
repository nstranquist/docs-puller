//go:build windows

package main

import (
	"os"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// Windows uses a bounded in-memory read for the flat embedding sidecar. This
// keeps the same read-only contract without unsafe view-to-slice conversion.
func mmapReadOnly(path string) ([]byte, func(), error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 {
		return nil, nil, searchruntime.EmbeddingFlatIndexVectorFileEmptyError(path)
	}
	return data, func() {}, nil
}
