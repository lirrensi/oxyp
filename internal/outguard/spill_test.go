package outguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func spillPathFromNote(note string) string {
	const marker = "full output in "
	i := strings.Index(note, marker)
	if i < 0 {
		return ""
	}
	rest := note[i+len(marker):]
	if j := strings.Index(rest, "]"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func removeSpill(p string) error { return os.Remove(p) }

func TestSpillNoteAndFileExact(t *testing.T) {
	// Text with NUL + invalid UTF-8 + trailing newline must round-trip exactly.
	text := "line1\nline2\x00\xff\xfe-end\n"
	blocks := []mcp.Content{mcp.TextContent{Type: "text", Text: text}}
	out, spilled := Apply(blocks, Limits{MaxBytes: 4, MaxLines: 2000})
	if !spilled {
		t.Fatal("must spill")
	}
	got := out[0].(mcp.TextContent).Text
	noteIdx := strings.Index(got, "\n…[output guard:")
	if noteIdx < 0 {
		t.Fatalf("missing guard note: %q", got)
	}
	preview := got[:noteIdx]
	skipped, err := parseSkipped(got[noteIdx:])
	if err != nil {
		t.Fatalf("note missing byte count: %q", got)
	}
	if skipped != len(text)-len(preview) {
		t.Fatalf("note skipped=%d, want %d", skipped, len(text)-len(preview))
	}
	path := spillPathFromNote(got)
	if path == "" {
		t.Fatalf("note missing spill path: %q", got)
	}
	if base := filepath.Base(path); !strings.HasPrefix(base, "oxyp-output-") || !strings.HasSuffix(base, ".txt") {
		t.Fatalf("spill path shape wrong: %q", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file unreadable: %v", err)
	}
	if string(b) != text {
		t.Fatalf("spill content mismatch: got %q want %q", b, text)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("spill mode = %o, want 600", st.Mode().Perm())
	}
	if dir := filepath.Dir(path); !isSpillDirRetained(dir) {
		t.Fatalf("spill dir unexpected: %q", dir)
	}
	_ = os.Remove(path)
}

type errString string

func (e errString) Error() string { return string(e) }

func parseSkipped(note string) (int, error) {
	const prefix = "showing head, "
	i := strings.Index(note, prefix)
	if i < 0 {
		return 0, errString("no skipped count")
	}
	s := note[i+len(prefix):]
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 || !strings.HasPrefix(s[j:], " bytes skipped") {
		return 0, errString("no skipped count")
	}
	v := 0
	for _, c := range s[:j] {
		v = v*10 + int(c-'0')
	}
	return v, nil
}

func isSpillDirRetained(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

func TestSpillFailureNote(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist", "sub"))
	blocks := []mcp.Content{mcp.TextContent{Type: "text", Text: strings.Repeat("x", 1000)}}
	out, spilled := Apply(blocks, Limits{MaxBytes: 10, MaxLines: 2000})
	if !spilled {
		t.Fatal("spill attempt with bad TMPDIR must still report spilled")
	}
	got := out[0].(mcp.TextContent).Text
	if !strings.Contains(got, "spill failed:") {
		t.Fatalf("expected 'spill failed:' note, got %q", got[:200])
	}
	if strings.Contains(got, "full output in ") {
		t.Fatalf("failed spill must not claim a path: %q", got)
	}
	// Preview is retained before the note.
	noteIdx := strings.Index(got, "\n…[output guard:")
	if got[:noteIdx] != strings.Repeat("x", 10) {
		t.Fatalf("preview not retained: %q", got[:noteIdx])
	}
}

func TestPruneAgeAndUnrelated(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, content string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if age > 0 {
			old := time.Now().Add(-age)
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("oxyp-output-old.txt", "old", 25*time.Hour)
	mk("oxyp-output-fresh.txt", "fresh", 0)
	mk("other.txt", "x", 25*time.Hour)
	mk("oxyp-output-nodot", "x", 25*time.Hour)
	mk("oxyp-output-.tmp", "x", 25*time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "oxyp-output-somedir.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	pruneSpills(dir)
	exists := func(n string) bool {
		_, err := os.Stat(filepath.Join(dir, n))
		return err == nil
	}
	if exists("oxyp-output-old.txt") {
		t.Error("25h-old spill file must be pruned")
	}
	for _, n := range []string{"oxyp-output-fresh.txt", "other.txt", "oxyp-output-nodot", "oxyp-output-.tmp"} {
		if !exists(n) {
			t.Errorf("%s must survive pruning", n)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, "oxyp-output-somedir.txt")); err != nil || !st.IsDir() {
		t.Error("matching directory must be left alone")
	}
}

func TestPruneMissingDir(t *testing.T) {
	// Nonexistent dir: no panic, no error (best-effort).
	pruneSpills(filepath.Join(t.TempDir(), "no-such-dir"))
	// File path as dir: ReadDir fails → no panic.
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneSpills(p)
}

func TestCountLinesSemantics(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1}, // trailing newline terminates, not a new line
		{"\n", 1},
		{"a\nb\nc", 3},
		{"a\nb\nc\n", 3},
	}
	for _, tc := range cases {
		if got := countLines(tc.in); got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
