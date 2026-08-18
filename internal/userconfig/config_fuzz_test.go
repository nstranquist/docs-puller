package userconfig

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzConfigParse drives the operator-config parser with arbitrary bytes. The
// config file (DOCS_PULLER_CONFIG, or ~/.docs-puller/config.yaml) is optional and
// operator-supplied, and loadFromDisk decodes it straight into Config via
// yaml.Unmarshal — this fuzzes exactly that decode. YAML is a much larger attack
// surface than the flat schema suggests: anchors, aliases, merge keys, and deep
// block nesting all run in the decoder before a single field is populated.
//
// Invariants: no panic on decode for any input; and any config that decodes must
// re-marshal without panicking, i.e. the decoded shape stays serialisable (as it
// must be for round-tripping and diagnostics).
func FuzzConfigParse(f *testing.F) {
	seeds := []string{
		"",
		"{}",
		"profiles_dir: ~/profiles\n",
		"cwd_profiles:\n  - profile: go\n    roots:\n      - ~/dev\n",
		"pin_scan_roots:\n  - ~/dev\n  - ~/tools\n",
		"tools_pin_scopes:\n  - path_contains: nicos-tools\n    scope: internal\n",
		"source_keywords:\n  react:\n    - hooks\n    - jsx\n",
		"profiles_dir: [not, a, string]\n", // type mismatch: decode error
		"a: &x [1]\nb: *x\n",               // anchor/alias
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return // a malformed config is rejected by Load, not a crash.
		}
		if _, err := yaml.Marshal(cfg); err != nil {
			t.Fatalf("a config that decoded cleanly failed to re-marshal: %v", err)
		}
	})
}
