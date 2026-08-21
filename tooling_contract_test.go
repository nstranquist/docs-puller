package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestStaticcheckPinIsSemverAndSingleSource(t *testing.T) {
	body, err := os.ReadFile(".staticcheck-version")
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.TrimSpace(string(body))
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`).MatchString(pin) {
		t.Fatalf("Staticcheck pin is not SemVer: %q", pin)
	}

	for _, path := range []string{"Makefile", ".github/workflows/ci.yml"} {
		consumer, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(consumer)
		if !strings.Contains(text, ".staticcheck-version") {
			t.Fatalf("%s does not consume the shared Staticcheck pin", path)
		}
		if regexp.MustCompile(`honnef\.co/go/tools/cmd/staticcheck@v[0-9]`).MatchString(text) {
			t.Fatalf("%s duplicates a literal Staticcheck version", path)
		}
	}
}
