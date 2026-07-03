package searchruntime

import (
	"os"
	"path/filepath"
	"testing"
)

// writeNdevStub installs a fake `ndev` on a fresh PATH dir that answers
// `secrets get <NAME>` from the given map (exit 0) and exits 1 otherwise — so
// the fallback is tested without touching the real keychain.
func writeNdevStub(t *testing.T, returns map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	sb := "#!/bin/sh\nif [ \"$1\" = secrets ] && [ \"$2\" = get ]; then\n  case \"$3\" in\n"
	for name, val := range returns {
		sb += "    " + name + ") printf '%s\\n' '" + val + "'; exit 0;;\n"
	}
	sb += "  esac\n  exit 1\nfi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ndev"), []byte(sb), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSecretViaNdev(t *testing.T) {
	dir := writeNdevStub(t, map[string]string{"MY_KEY": "sk-keychain"})
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := SecretViaNdev("MY_KEY"); got != "sk-keychain" {
		t.Errorf("SecretViaNdev(MY_KEY) = %q, want sk-keychain", got)
	}
	if got := SecretViaNdev("UNKNOWN"); got != "" {
		t.Errorf("SecretViaNdev(UNKNOWN) = %q, want empty", got)
	}
	if got := SecretViaNdev(""); got != "" {
		t.Errorf("SecretViaNdev(empty) = %q, want empty", got)
	}
}

func TestResolveEmbeddingAPIKey_EnvWins(t *testing.T) {
	dir := writeNdevStub(t, map[string]string{DefaultEmbeddingAPIKeyEnv: "sk-keychain"})
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(DefaultEmbeddingAPIKeyEnv, "sk-env")
	if got := ResolveEmbeddingAPIKey(); got != "sk-env" {
		t.Errorf("env should win: got %q want sk-env", got)
	}
}

func TestResolveEmbeddingAPIKey_SecretsFallback(t *testing.T) {
	dir := writeNdevStub(t, map[string]string{DefaultEmbeddingAPIKeyEnv: "sk-keychain"})
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(DefaultEmbeddingAPIKeyEnv, "") // empty env → keychain fallback
	if got := ResolveEmbeddingAPIKey(); got != "sk-keychain" {
		t.Errorf("secrets fallback: got %q want sk-keychain", got)
	}
}
