package main

import (
	_ "embed"
	"sync"

	"github.com/nstranquist/docs-puller/internal/releasecontract"
)

//go:embed release/manifest.json
var embeddedReleaseManifest []byte

var (
	releaseManifestOnce sync.Once
	releaseManifestData releasecontract.Manifest
)

func releaseManifest() releasecontract.Manifest {
	releaseManifestOnce.Do(func() {
		manifest, err := releasecontract.Parse(embeddedReleaseManifest)
		if err != nil {
			panic(err)
		}
		releaseManifestData = manifest
	})
	return releaseManifestData
}
