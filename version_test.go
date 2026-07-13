package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCurrentVersionInfoPublishesStableContract(t *testing.T) {
	info := currentVersionInfo()
	if info.SchemaVersion != 1 || info.ContractVersion != cliContractVersion {
		t.Fatalf("contract versions = schema %d contract %d", info.SchemaVersion, info.ContractVersion)
	}
	if info.Name != "docs-puller" || info.Module == "" || info.Version == "" {
		t.Fatalf("incomplete version info: %+v", info)
	}
	for _, command := range []string{"pull", "search", "status", "version"} {
		if !slices.Contains(info.Commands, command) {
			t.Errorf("commands missing %q: %v", command, info.Commands)
		}
	}
	for _, capability := range []string{"contract.version-json.v1", "embed.stale-prune.v1", "pull.llms-txt", "pull.replace-source-guard.v1", "search.fts5", "search.fts5.self-heal.v1", "search.hybrid-source-scope.v1", "telemetry.provenance.v1"} {
		if !slices.Contains(info.Capabilities, capability) {
			t.Errorf("capabilities missing %q: %v", capability, info.Capabilities)
		}
	}
}

func TestCheckExpectedVersion(t *testing.T) {
	if err := checkExpectedVersion("v0.2.0", "v0.2.0"); err != nil {
		t.Fatalf("matching version rejected: %v", err)
	}
	if err := checkExpectedVersion("v0.2.0", ""); err != nil {
		t.Fatalf("empty expectation rejected: %v", err)
	}
	err := checkExpectedVersion("devel", "v0.2.0")
	if err == nil || !strings.Contains(err.Error(), `got "devel", want "v0.2.0"`) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestTopLevelUsageAdvertisesVersionExpectationGate(t *testing.T) {
	if !strings.Contains(topLevelUsage, "docs-puller version [--json] [--expect VERSION]") {
		t.Fatalf("top-level usage does not advertise the exact-version gate:\n%s", topLevelUsage)
	}
}
