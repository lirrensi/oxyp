package outguard

import (
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestPassthroughUnderLimit(t *testing.T) {
	blocks := []mcp.Content{
		mcp.TextContent{Type: "text", Text: "hello"},
		mcp.ImageContent{Type: "image", Data: "QUJD", MIMEType: "image/png"},
	}
	out, spilled := Apply(blocks, Limits{MaxBytes: 100, MaxLines: 10})
	if spilled || len(out) != 2 {
		t.Fatalf("small content must pass: spilled=%v", spilled)
	}
	if out[0].(mcp.TextContent).Text != "hello" {
		t.Fatal("text mangled")
	}
	if out[1].(mcp.ImageContent).Data != "QUJD" {
		t.Fatal("media touched")
	}
}

func TestSpillsBigText(t *testing.T) {
	big := strings.Repeat("x", 1000)
	blocks := []mcp.Content{mcp.TextContent{Type: "text", Text: big}}
	out, spilled := Apply(blocks, Limits{MaxBytes: 100, MaxLines: 2000})
	if !spilled {
		t.Fatal("big text must spill")
	}
	got := out[0].(mcp.TextContent).Text
	if !strings.Contains(got, "output guard") || !strings.Contains(got, "oxyp-output-") {
		t.Fatalf("missing guard note: %q", got[:120])
	}
	// spill file exists, 0600, full content
	path := strings.Split(strings.Split(got, "full output in ")[1], "]")[0]
	b, err := os.ReadFile(path)
	if err != nil || string(b) != big {
		t.Fatalf("spill file bad: %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("spill mode = %o", st.Mode().Perm())
	}
	_ = os.Remove(path)
}

func TestLineLimitAndMediaUntouched(t *testing.T) {
	many := strings.Repeat("l\n", 50)
	huge := strings.Repeat("Z", 5000) // fake base64: must NOT be cut
	blocks := []mcp.Content{
		mcp.TextContent{Type: "text", Text: many},
		mcp.ImageContent{Type: "image", Data: huge, MIMEType: "image/png"},
	}
	out, spilled := Apply(blocks, Limits{MaxBytes: 1 << 20, MaxLines: 10})
	if !spilled {
		t.Fatal("many lines must spill")
	}
	if got := out[0].(mcp.TextContent).Text; strings.Count(got, "\n") > 12 {
		t.Fatal("line cap not applied")
	}
	if out[1].(mcp.ImageContent).Data != huge {
		t.Fatal("media block truncated — forbidden")
	}
}

func TestSpillRetentionBounded(t *testing.T) {
	dir := t.TempDir()
	// seed 105 spill files
	for i := range 105 {
		f, err := os.CreateTemp(dir, "oxyp-output-*.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(strings.Repeat("x", 10) + string(rune('a'+i%26)))
		_ = f.Close()
	}
	pruneSpills(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 100 {
		t.Fatalf("spill dir unbounded: %d files", len(entries))
	}
}
