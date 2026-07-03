package main

import "testing"

func TestLintCurationHasNoErrors(t *testing.T) {
	report := lintCuration()
	if len(report.Errors) > 0 {
		t.Fatalf("curation errors: %v", report.Errors)
	}
}
