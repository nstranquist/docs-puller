package sourcehygiene

import "testing"

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
