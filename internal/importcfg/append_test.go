package importcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendBlocksSeparators(t *testing.T) {
	mk := func() string {
		text, err := RenderBlock("s", mustEntry(t, "cursor", RawEntry{Command: []string{"x"}}), nil)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	t.Run("missingFile", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := AppendBlocks(filepath.Join(dir, "sub"), mk()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("emptyExistingLeadingBlankCurrent", func(t *testing.T) {
		// current behavior: appending to an existing EMPTY file leaves a
		// leading blank line. Pinned (new files don't have it).
		dir := t.TempDir()
		if err := os.WriteFile(ConfigPath(dir), []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AppendBlocks(dir, mk()); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(ConfigPath(dir))
		if !strings.HasPrefix(string(b), "\n[servers.s]") {
			t.Fatalf("leading blank missing: %q", string(b)[:20])
		}
	})
	t.Run("trailingNewline", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(ConfigPath(dir), []byte("[x]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AppendBlocks(dir, mk()); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(ConfigPath(dir))
		if !strings.Contains(string(b), "[x]\n\n[servers.s]") {
			t.Fatalf("separator wrong: %q", string(b)[:30])
		}
	})
	t.Run("noTrailingNewline", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(ConfigPath(dir), []byte("[x]"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AppendBlocks(dir, mk()); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(ConfigPath(dir))
		if !strings.Contains(string(b), "[x]\n\n[servers.s]") {
			t.Fatalf("separator wrong: %q", string(b)[:30])
		}
	})
	t.Run("textWithoutNewline", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := AppendBlocks(dir, "[servers]\n  [servers.a]\n    x = 1"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(ConfigPath(dir))
		if !strings.HasSuffix(string(b), "x = 1\n") {
			t.Fatalf("tail = %q", string(b))
		}
	})
	t.Run("emptyText", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := AppendBlocks(dir, ""); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(ConfigPath(dir))
		if string(b) != "\n" {
			t.Fatalf("empty append = %q", string(b))
		}
	})
	t.Run("nestedDirs", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "b")
		if _, err := AppendBlocks(dir, mk()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(ConfigPath(dir)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAppendBlocksModesAndNotes(t *testing.T) {
	mk := func() string {
		text, err := RenderBlock("s", mustEntry(t, "cursor", RawEntry{Command: []string{"x"}}), nil)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	dir := t.TempDir()
	if _, err := AppendBlocks(dir, mk()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(ConfigPath(dir)); st.Mode().Perm() != 0o600 {
		t.Fatalf("new mode = %o", st.Mode().Perm())
	}
	dir2 := t.TempDir()
	if _, err := AppendBlocks(dir2, mk()); err != nil {
		t.Fatal(err)
	}
	note, err := AppendBlocks(dir2, mk())
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("0600 existing must not note: %q", note)
	}
	if err := os.Chmod(ConfigPath(dir2), 0o644); err != nil {
		t.Fatal(err)
	}
	note, err = AppendBlocks(dir2, mk())
	if err != nil {
		t.Fatal(err)
	}
	if note == "" || !strings.Contains(note, "tightened") {
		t.Fatalf("want tighten note, got %q", note)
	}
	if st, _ := os.Stat(ConfigPath(dir2)); st.Mode().Perm() != 0o600 {
		t.Fatalf("tightened mode = %o", st.Mode().Perm())
	}
}
