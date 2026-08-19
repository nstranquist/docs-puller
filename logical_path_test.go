package main

import "testing"

func TestCleanLogicalRelativePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "slash path", raw: "docs/guides/start.md", want: "docs/guides/start.md", ok: true},
		{name: "windows separators", raw: `docs\guides\start.md`, want: "docs/guides/start.md", ok: true},
		{name: "clean segments", raw: "docs/./guides/../start.md", want: "docs/start.md", ok: true},
		{name: "empty", raw: "", ok: false},
		{name: "current directory", raw: ".", ok: false},
		{name: "parent", raw: "../secret.md", ok: false},
		{name: "backslash parent", raw: `..\secret.md`, ok: false},
		{name: "posix absolute", raw: "/etc/passwd", ok: false},
		{name: "windows absolute", raw: `C:\Windows\win.ini`, ok: false},
		{name: "windows drive relative", raw: `C:secret.md`, ok: false},
		{name: "unc", raw: `\\server\share\secret.md`, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := cleanLogicalRelativePath(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("cleanLogicalRelativePath(%q) = %q, %v; want %q, %v", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}
