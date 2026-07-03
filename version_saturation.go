package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nstranquist/docs-puller/searchruntime"
)

const (
	versionLaneArchived = "archived"
	versionLaneNext     = "next"
)

var reactNativePathVersionRE = regexp.MustCompile(`^\d+\.\d+(?:\.\d+)?$`)

func searchCandidateHygieneLimit(query string, o searchOpts) int {
	limit := searchruntime.ExpandedSourceHygieneLimit(o.limit)
	if collapse := versionCollapseCandidateLimit(query, o); collapse > limit {
		return collapse
	}
	return limit
}

func postRetrievalUserLimit(query string, pre searchruntime.PreRetrievalPlan, o searchOpts) int {
	if shouldCollapseVersionEquivalentStems(query, o) {
		return pre.BM25Limit
	}
	return pre.UserLimit
}

func versionCollapseCandidateLimit(query string, o searchOpts) int {
	if !shouldCollapseVersionEquivalentStems(query, o) || o.limit <= 0 {
		return 0
	}
	expanded := o.limit * 20
	if floor := o.limit + 50; expanded < floor {
		expanded = floor
	}
	if expanded > 200 {
		return 200
	}
	return expanded
}

func shouldCollapseVersionEquivalentStems(query string, o searchOpts) bool {
	if o.allVersions || strings.TrimSpace(o.version) != "" {
		return false
	}
	source := o.requestedSource
	if source == "" {
		source = o.source
	}
	if source == "react-native" {
		return true
	}
	q := strings.ToLower(query)
	return strings.Contains(q, "react native") || strings.Contains(q, "react-native")
}

func collapseVersionEquivalentStems(hits []searchHit, limit int) []searchHit {
	if limit <= 0 || len(hits) <= 1 {
		return hits
	}
	out := make([]searchHit, 0, minInt(limit, len(hits)))
	seen := make(map[string]bool, len(hits))
	for _, hit := range hits {
		stem := hit.Path
		if versionStem, ok := reactNativeVersionEquivalentStem(hit); ok {
			stem = versionStem
		}
		if seen[stem] {
			continue
		}
		seen[stem] = true
		out = append(out, hit)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

func reactNativeVersionEquivalentStem(hit searchHit) (string, bool) {
	family := hit.SourceFamily
	if family == "" {
		family = hit.Source
	}
	if family != "react-native" {
		return "", false
	}
	rel, ok := strings.CutPrefix(filepath.ToSlash(hit.Path), family+"/")
	if !ok {
		return "", false
	}
	segments := strings.Split(rel, "/")
	if len(segments) < 2 || segments[0] != "docs" {
		return "", false
	}
	if len(segments) >= 3 && isReactNativePathVersionSegment(segments[1]) {
		segments = append([]string{segments[0]}, segments[2:]...)
	}
	return family + "/" + strings.Join(segments, "/"), true
}

func sourceInfoForHit(hit searchHit, pins *docsPinsFileData, reg *versionPolicyRegistry) versionPolicySourceInfo {
	info := sourceInfoForSource(hit.Source, pins, reg)
	pageInfo, ok := reactNativePageVersionInfo(hit.Path, info.VersionPolicySourceInfo, reg)
	if !ok {
		return info
	}
	info.VersionPolicySourceInfo = pageInfo
	info.Pin = nil
	return info
}

func reactNativePageVersionInfo(path string, base searchruntime.VersionPolicySourceInfo, reg *versionPolicyRegistry) (searchruntime.VersionPolicySourceInfo, bool) {
	family := base.SourceFamily
	if family == "" {
		family = base.SourceID
	}
	if family != "react-native" {
		return searchruntime.VersionPolicySourceInfo{}, false
	}
	rel, ok := strings.CutPrefix(filepath.ToSlash(path), family+"/")
	if !ok {
		return searchruntime.VersionPolicySourceInfo{}, false
	}
	segments := strings.Split(rel, "/")
	if len(segments) < 3 || segments[0] != "docs" || !isReactNativePathVersionSegment(segments[1]) {
		return searchruntime.VersionPolicySourceInfo{}, false
	}
	version := reactNativeDocsVersionKey(segments[1], reg)
	info := base
	info.SourceFamily = family
	info.SourceID = pinnedSourceID(family, version)
	info.DocsVersion = version
	info.VersionLane = versionLaneArchived
	if segments[1] == "next" {
		info.VersionLane = versionLaneNext
	}
	return info, true
}

func sourceHasPathVersion(out, family, versionKey string) bool {
	if family != "react-native" || versionKey == "" || !isReactNativePathVersionSegment(versionKey) {
		return false
	}
	info, err := os.Stat(filepath.Join(out, family, "docs", versionKey))
	return err == nil && info.IsDir()
}

func reactNativeDocsVersionKey(version string, reg *versionPolicyRegistry) string {
	if version == "next" {
		return version
	}
	strategy := "semver_minor"
	if reg != nil {
		if src, ok := reg.byID["react-native"]; ok && src.DocsVersionStrategy != "" {
			strategy = src.DocsVersionStrategy
		}
	}
	if key := docsVersionKey(version, strategy); key != "" {
		return key
	}
	return cleanResolvedVersion(version)
}

func isReactNativePathVersionSegment(segment string) bool {
	return segment == "next" || reactNativePathVersionRE.MatchString(segment)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
