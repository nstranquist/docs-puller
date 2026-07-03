package main

import "testing"

func TestListProfiles_EmbeddedExampleOnly(t *testing.T) {
	names := ListProfiles(t.TempDir())
	found := false
	for _, n := range names {
		if n == "example" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListProfiles missing 'example': %v", names)
	}
}
