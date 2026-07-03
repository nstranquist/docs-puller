package sourcehygiene

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed policy.json
var policyBytes []byte

// Pattern is one path-pattern rule in the shared retrieval hygiene policy.
type Pattern struct {
	Pattern        string `json:"pattern"`
	Reason         string `json:"reason"`
	Penalty        int    `json:"penalty"`
	ExcludeContext bool   `json:"exclude_context"`
}

type policy struct {
	Version  int       `json:"version"`
	Patterns []Pattern `json:"patterns"`
}

// Classification is the shared source-hygiene verdict for one path-like value.
type Classification struct {
	Penalty        int
	ExcludeContext bool
	Reason         string
	Pattern        string
}

var (
	policyOnce   sync.Once
	loadedPolicy policy
)

func getPolicy() policy {
	policyOnce.Do(func() { loadedPolicy = loadPolicy() })
	return loadedPolicy
}

// loadPolicy parses the embedded policy and, when DOCS_PULLER_HYGIENE_POLICY
// names a JSON file of the same shape, appends its patterns. User patterns
// extend (never replace) the embedded set. A broken user file warns and is
// skipped — ranking hygiene must never take down search.
func loadPolicy() policy {
	var p policy
	if err := json.Unmarshal(policyBytes, &p); err != nil {
		panic(err)
	}
	path := os.Getenv("DOCS_PULLER_HYGIENE_POLICY")
	if path == "" {
		return p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sourcehygiene: skipping user policy %s: %v\n", path, err)
		return p
	}
	var user policy
	if err := json.Unmarshal(data, &user); err != nil {
		fmt.Fprintf(os.Stderr, "sourcehygiene: skipping malformed user policy %s: %v\n", path, err)
		return p
	}
	p.Patterns = append(p.Patterns, user.Patterns...)
	return p
}

// Classify returns the strongest matching source-hygiene verdict for any
// path-like value. Inputs may include ids, paths, URLs, or display paths.
func Classify(values ...string) Classification {
	haystack := normalize(strings.Join(values, "\n"))
	var out Classification
	for _, pattern := range getPolicy().Patterns {
		if pattern.Pattern == "" || !strings.Contains(haystack, normalize(pattern.Pattern)) {
			continue
		}
		if pattern.Penalty > out.Penalty || (pattern.Penalty == out.Penalty && out.Reason == "") {
			out = Classification{
				Penalty:        pattern.Penalty,
				ExcludeContext: pattern.ExcludeContext,
				Reason:         pattern.Reason,
				Pattern:        pattern.Pattern,
			}
		}
	}
	return out
}

// Penalty is a compact helper for ranking call sites.
func Penalty(values ...string) int {
	return Classify(values...).Penalty
}

// ExpandedLimit returns an overfetch limit large enough to let callers
// downrank generated/replay hits without losing durable hits below them.
func ExpandedLimit(limit int) int {
	if limit <= 0 {
		return limit
	}
	expanded := limit * 5
	if expanded < limit+10 {
		expanded = limit + 10
	}
	if expanded > 50 {
		return 50
	}
	return expanded
}

func normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return filepath.ToSlash(value)
}
