package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPassageIntegrityAndBudget(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "demo"), 0700)
	path := filepath.Join(root, "demo", "a.md")
	body := "# API\nintro\n```go\nfunc main() {\n println(1)\n}\n```\nend\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	req := []Request{{Path: "demo/a.md", Line: 4, Lines: 1}}
	p, err := Build(context.Background(), root, req, 1000)
	if err != nil || len(p.Passages) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	a := p.Passages[0]
	if a.StartLine != 3 || a.EndLine != 7 || !strings.HasPrefix(a.Text, "```go\n") || !strings.HasSuffix(a.Text, "\n```") {
		t.Fatalf("broken fence: %+v", a)
	}
	if len(a.Headings) != 1 || a.Headings[0] != "API" {
		t.Fatal(a.Headings)
	}
	p, _ = Build(context.Background(), root, append(req, req...), 1000)
	if len(p.Passages) != 1 {
		t.Fatal("not deduplicated")
	}
	p, _ = Build(context.Background(), root, req, 2)
	if len(p.Passages) != 0 || len(p.Skipped) != 1 {
		t.Fatal("budget exceeded")
	}
	os.WriteFile(path, []byte(body+"changed\n"), 0600)
	p, _ = Build(context.Background(), root, req, 1000)
	if p.Passages[0].ID == a.ID || p.Passages[0].ContentHash == a.ContentHash || p.Passages[0].DocumentID != a.DocumentID {
		t.Fatal("content identity did not change independently of document identity")
	}
}

func TestConfinedReadsAndCancellation(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret.md", filepath.Join(outside, "secret.md"), "escape/secret.md"} {
		p, err := Build(context.Background(), root, []Request{{Path: path}}, 100)
		if err != nil || len(p.Passages) != 0 || len(p.Skipped) != 1 {
			t.Fatalf("unsafe path %s: %+v %v", path, p, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, root, []Request{{Path: "a.md"}}, 100); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestRejectOversizedAndInvalidDocuments(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		body []byte
	}{{"large.md", []byte(strings.Repeat("x", MaxDocumentBytes+1))}, {"bad.md", []byte{0xff}}} {
		os.WriteFile(filepath.Join(root, tc.name), tc.body, 0600)
		p, err := Build(context.Background(), root, []Request{{Path: tc.name}}, 100)
		if err != nil || len(p.Passages) != 0 || len(p.Skipped) != 1 {
			t.Fatalf("%s: %+v %v", tc.name, p, err)
		}
	}
}

func TestHeadingPathWithSkippedLevels(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.md"), []byte("# Root\n### First\ntext\n### Second\nbody\n"), 0600)
	p, err := Build(context.Background(), root, []Request{{Path: "a.md", Line: 5, Lines: 1}}, 1000)
	if err != nil || len(p.Passages) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	got := strings.Join(p.Passages[0].Headings, "/")
	if got != "Root/Second" {
		t.Fatalf("heading path=%q", got)
	}
}

func TestExtremeWindowCannotOverflow(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.md"), []byte("first\nsecond\nthird\n"), 0600)
	p, err := Build(context.Background(), root, []Request{{Path: "a.md", Line: 2, Lines: int(^uint(0) >> 1)}}, 1000)
	if err != nil || len(p.Passages) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	if p.Passages[0].EndLine != 3 {
		t.Fatal(p.Passages[0])
	}
}
