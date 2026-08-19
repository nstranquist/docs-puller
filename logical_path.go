package main

import (
	pathpkg "path"
	"strings"
)

// cleanLogicalRelativePath normalizes a repository or corpus path to the
// slash-separated form used in manifests, APIs, and search results. It rejects
// POSIX absolute paths, Windows drive paths, UNC paths, and traversal on every
// host OS so validation does not change with the machine that runs it.
func cleanLogicalRelativePath(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", false
	}
	normalized := strings.ReplaceAll(raw, `\`, "/")
	if pathpkg.IsAbs(normalized) || hasWindowsDrivePrefix(normalized) {
		return "", false
	}
	clean := pathpkg.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// canonicalLogicalPath converts a corpus path to its platform-independent
// representation. Corpus paths are identifiers, not filesystem paths: they
// are persisted in manifests and returned by APIs, so they always use '/'.
func canonicalLogicalPath(raw string) string {
	return strings.ReplaceAll(raw, `\`, "/")
}

func hasWindowsDrivePrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	first := path[0]
	return (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')
}
