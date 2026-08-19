package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nstranquist/docs-puller/searchruntime"
)

// acquireWriteLock blocks until it holds an exclusive flock on
// <out>/.cache/.write-lock. Used to serialize the post-pull critical section
// (writeManifests + regenerateIndex + FTS5 rebuild) across concurrent pull
// invocations.
//
// Without this, two parallel `docs-puller pull` runs race on the FTS5 db and
// the second writer hits SQLITE_BUSY mid-rebuild — the index ends up in an
// inconsistent state until the next manual `reindex`. Concurrency was real:
// during the 4-source parallel pull session we observed exactly this failure.
//
// Returns a release closure the caller MUST defer.
func acquireWriteLock(out string) (release func(), err error) {
	cacheDir := filepath.Join(out, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(cacheDir, ".write-lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, searchruntime.WriteLockOpenError(err)
	}
	unlock, err := lockFile(f)
	if err != nil {
		f.Close()
		return nil, searchruntime.WriteLockFlockError(err)
	}
	return func() {
		_ = unlock()
		_ = f.Close()
	}, nil
}

// withWriteLock runs fn while holding the write lock. Logs wait time when
// non-trivial so users see when their pull was blocked behind another.
func withWriteLock(out string, fn func() error) error {
	start := time.Now()
	release, err := acquireWriteLock(out)
	if err != nil {
		return err
	}
	defer release()
	if waited := time.Since(start); waited > 500*time.Millisecond {
		fmt.Fprintf(os.Stderr, "(waited %s for concurrent pull to finish)\n", waited.Round(100*time.Millisecond))
	}
	return fn()
}
