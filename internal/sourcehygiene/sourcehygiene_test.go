package sourcehygiene

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyGeneratedAndScratchPaths(t *testing.T) {
	cases := []struct {
		path   string
		reason string
	}{
		{
			path:   "/home/user/code/example-monorepo/hermes/evals/runs/20260423/transcript.txt",
			reason: "hermes-eval-log",
		},
		{
			path:   "/home/user/notes/team-kb/generated/local-ai/human-command-prompts/exports/codex/2026-04-part-14.md",
			reason: "generated-human-command-prompt-export",
		},
		{
			path:   "/home/user/code/example-monorepo/cmd/docs-puller/eval/run/stdout.json",
			reason: "docs-puller-eval-artifact",
		},
		{
			path:   "/home/user/code/example-monorepo/.claude/worktrees/agent/evals/case.md",
			reason: "agent-scratch-worktree",
		},
	}
	for _, tc := range cases {
		got := Classify(tc.path)
		if got.Penalty == 0 || !got.ExcludeContext || got.Reason != tc.reason {
			t.Fatalf("Classify(%q) = %#v, want reason %q and context exclusion", tc.path, got, tc.reason)
		}
	}
}

func TestClassifyDurablePathClean(t *testing.T) {
	got := Classify("tools-dev/docs/context-retrieval/README.md")
	if got.Penalty != 0 || got.ExcludeContext || got.Reason != "" {
		t.Fatalf("durable context doc classified as noisy: %#v", got)
	}
}

func TestExpandedLimit(t *testing.T) {
	if got := ExpandedLimit(3); got != 15 {
		t.Fatalf("ExpandedLimit(3) = %d, want 15", got)
	}
	if got := ExpandedLimit(20); got != 50 {
		t.Fatalf("ExpandedLimit(20) = %d, want cap 50", got)
	}
}

func TestLoadPolicyMergesUserPatterns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	user := `{"version":1,"patterns":[{"pattern":"my-scratch-notes/","reason":"user-scratch","penalty":20,"exclude_context":true}]}`
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_HYGIENE_POLICY", path)

	p := loadPolicy()
	found := false
	for _, pat := range p.Patterns {
		if pat.Pattern == "my-scratch-notes/" && pat.Reason == "user-scratch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user pattern not merged; patterns = %d", len(p.Patterns))
	}
	if len(p.Patterns) < 2 {
		t.Fatal("embedded patterns must be retained alongside user patterns")
	}
}

func TestLoadPolicySkipsMalformedUserFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_HYGIENE_POLICY", path)

	p := loadPolicy()
	if len(p.Patterns) == 0 {
		t.Fatal("embedded policy must survive a malformed user file")
	}
}
