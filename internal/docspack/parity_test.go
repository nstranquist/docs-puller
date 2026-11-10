package docspack

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Parity harness (not an assertion test): prints BuildPack stats for the
// same fixture set the Python parity run uses, for PARITY.md comparison.
func TestParityPrint(t *testing.T) {
	files := []string{"nav_heavy", "dup_heavy_a", "dup_heavy_b", "code_heavy"}
	var sources []SourceDoc
	for _, n := range files {
		sources = append(sources, SourceDoc{"http://x.test/" + n, loadFixture(t, n+".html")})
	}
	for _, budget := range []int{8000, 120} {
		_, stats, all := BuildPack(sources, budget)
		b, _ := json.Marshal(stats)
		fmt.Printf("GO budget %d %s\n", budget, b)
		per := map[string]int{}
		for _, c := range all {
			per[c.Source] += c.Tokens
		}
		pb, _ := json.Marshal(per)
		fmt.Printf("GO chunk tokens per source: %s\n", pb)
	}
}
