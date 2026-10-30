package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("OXYP_TEST_TOKEN", "s3cret")
	got, err := expandValue("Bearer ${OXYP_TEST_TOKEN}")
	if err != nil || got != "Bearer s3cret" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = expandValue("x-${NOPE_UNSET_12345}-y")
	if err != nil || got != "x-${NOPE_UNSET_12345}-y" {
		t.Fatalf("unset should stay literal: %q", got)
	}
}

func TestExpandFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	if err := os.WriteFile(p, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := expandValue("Bearer {file:" + p + "}")
	if err != nil || got != "Bearer file-secret" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := expandValue("{file:/nonexistent-xyz}"); err == nil {
		t.Fatal("want error for missing file")
	}
	if _, err := expandValue("{file:relative/path}"); err == nil {
		t.Fatal("want error for relative path")
	}
}

func TestRemoteFailClosedOnUnset(t *testing.T) {
	s := &ServerEntry{Name: "r", URL: "https://x/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${NOPE_UNSET_12345}"}}
	if msg := expandServerSecrets(s); msg == "" {
		t.Fatal("want fail-closed on unset header var")
	}
	s2 := &ServerEntry{Name: "r", URL: "https://${NOPE_UNSET_12345}/mcp"}
	if msg := expandServerSecrets(s2); msg == "" {
		t.Fatal("want fail-closed on unset url var")
	}
}
