package outguard

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func textOf(t *testing.T, b mcp.Content) string {
	t.Helper()
	tc, ok := b.(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", b)
	}
	return tc.Text
}

func TestExactBoundaries(t *testing.T) {
	// countLines("a\nb\nc") == 3, len 5.
	exact := "a\nb\nc"
	lim := Limits{MaxBytes: 5, MaxLines: 3}
	out, spilled := Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: exact}}, lim)
	if spilled || textOf(t, out[0]) != exact {
		t.Fatalf("exact-boundary text must pass through: spilled=%v %q", spilled, textOf(t, out[0]))
	}
	// +1 byte spills.
	out, spilled = Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: exact + "x"}}, lim)
	if !spilled {
		t.Fatal("+1 byte must spill")
	}
	// +1 line spills ("a\nb\nc\nd" has 4 lines).
	out, spilled = Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: "a\nb\nc\nd"}}, Limits{MaxBytes: 1 << 20, MaxLines: 3})
	if !spilled {
		t.Fatal("+1 line must spill")
	}
	// Empty text at zero limits passes (0 <= 0 on both).
	out, spilled = Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: ""}}, Limits{})
	if spilled || textOf(t, out[0]) != "" {
		t.Fatalf("empty text at zero limits must pass: spilled=%v", spilled)
	}
	// Non-empty at zero limits spills (0 MaxBytes/MaxLines cut everything).
	out, spilled = Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}, Limits{})
	if !spilled {
		t.Fatal("non-empty text at zero limits must spill")
	}
	if got := textOf(t, out[0]); !strings.HasPrefix(got, "\n…[output guard:") {
		t.Fatalf("zero-limit preview should be empty + note, got %q", got)
	}
}

func TestLineLimitTrailingNewline(t *testing.T) {
	// A trailing newline terminates the last line: "a\nb\nc\n" is 3
	// lines and passes MaxLines=3 without spilling.
	s := "a\nb\nc\n"
	out, spilled := Apply(
		[]mcp.Content{mcp.TextContent{Type: "text", Text: s}},
		Limits{MaxBytes: 1 << 20, MaxLines: 3},
	)
	if spilled {
		t.Fatalf("3-line text must pass MaxLines=3: %q", textOf(t, out[0]))
	}
	// 4 real lines still spill.
	out, spilled = Apply(
		[]mcp.Content{mcp.TextContent{Type: "text", Text: s + "d\n"}},
		Limits{MaxBytes: 1 << 20, MaxLines: 3},
	)
	if !spilled {
		t.Fatal("4-line text must spill at MaxLines=3")
	}
}

func TestPreviewRespectsBothLimits(t *testing.T) {
	s := "aaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\ndddddddddd\n" // 4 lines x 11 bytes
	// Line cut first, then byte cut.
	prev, skipped := head(s, Limits{MaxBytes: 1 << 20, MaxLines: 3})
	if prev != "aaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\n" {
		t.Fatalf("line cut wrong: %q", prev)
	}
	if skipped != len("dddddddddd\n") {
		t.Fatalf("skipped = %d, want %d", skipped, len("dddddddddd\n"))
	}
	if countLines(prev) > 3+1 { // current behavior: preview may hold MaxLines+1 by countLines
		t.Fatalf("preview holds %d lines, want <= MaxLines+1: %q", countLines(prev), prev)
	}
	// Both limits: line cut to 3 lines (33 bytes), then byte cut to 15.
	prev, skipped = head(s, Limits{MaxBytes: 15, MaxLines: 3})
	if len(prev) > 15 {
		t.Fatalf("byte cap not applied: %d bytes", len(prev))
	}
	if skipped != len(s)-len(prev) {
		t.Fatalf("skipped = %d, want %d", skipped, len(s)-len(prev))
	}
	// End-to-end: preview embedded before the note has no spill note inside it.
	out, spilled := Apply(
		[]mcp.Content{mcp.TextContent{Type: "text", Text: s}},
		Limits{MaxBytes: 15, MaxLines: 3},
	)
	if !spilled {
		t.Fatal("must spill")
	}
	got := textOf(t, out[0])
	noteIdx := strings.Index(got, "\n…[output guard:")
	if noteIdx < 0 {
		t.Fatalf("missing guard note: %q", got)
	}
	if body := got[:noteIdx]; len(body) > 15 {
		t.Fatalf("embedded preview exceeds byte cap: %q", body)
	}
}

func TestUTF8Boundary(t *testing.T) {
	s := strings.Repeat("é", 100) // 200 bytes, 2 bytes/rune
	prev, skipped := head(s, Limits{MaxBytes: 101, MaxLines: 1000})
	if !validUTF8(prev) {
		t.Fatalf("preview is not valid UTF-8: %q", prev)
	}
	if len(prev) > 101 {
		t.Fatalf("preview %d bytes exceeds cap 101", len(prev))
	}
	if len(prev) != 100 { // 101 is mid-rune, trimmed to 100
		t.Fatalf("preview len = %d, want 100 (rune-boundary trim)", len(prev))
	}
	if skipped != 100 {
		t.Fatalf("skipped = %d, want 100", skipped)
	}
	// Cap smaller than one rune → empty preview, everything skipped.
	prev, skipped = head(s, Limits{MaxBytes: 1, MaxLines: 1000})
	if prev != "" || skipped != len(s) {
		t.Fatalf("sub-rune cap: prev=%q skipped=%d", prev, skipped)
	}
	// End-to-end through Apply.
	out, spilled := Apply(
		[]mcp.Content{mcp.TextContent{Type: "text", Text: s}},
		Limits{MaxBytes: 101, MaxLines: 1000},
	)
	if !spilled {
		t.Fatal("must spill")
	}
	got := textOf(t, out[0])
	body := got[:strings.Index(got, "\n…[output guard:")]
	if !validUTF8(body) || len(body) > 101 {
		t.Fatalf("embedded preview invalid/over cap: %q", body)
	}
}

func validUTF8(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		var n int
		switch {
		case c < 0x80:
			n = 1
		case c>>5 == 0x6:
			n = 2
		case c>>4 == 0xE:
			n = 3
		case c>>3 == 0x1E:
			n = 4
		default:
			return false
		}
		if i+n > len(s) {
			return false
		}
		for _, b := range []byte(s[i+1 : i+n]) {
			if b>>6 != 0x2 {
				return false
			}
		}
		i += n
	}
	return true
}

func TestMultipleBlocksAndMedia(t *testing.T) {
	big1 := strings.Repeat("A", 500) + "\x00" + strings.Repeat("B", 500)
	big2 := strings.Repeat("z", 2000)
	small := "tiny"
	img := strings.Repeat("Z", 5000)
	aud := "AUDIODATA"
	in := []mcp.Content{
		mcp.TextContent{Type: "text", Text: big1},
		mcp.TextContent{Type: "text", Text: small},
		mcp.ImageContent{Type: "image", Data: img, MIMEType: "image/png"},
		mcp.AudioContent{Type: "audio", Data: aud, MIMEType: "audio/mp3"},
		mcp.TextContent{Type: "text", Text: big2},
	}
	lim := Limits{MaxBytes: 100, MaxLines: 2000}
	out, spilled := Apply(in, lim)
	if !spilled {
		t.Fatal("must spill")
	}
	if len(out) != len(in) {
		t.Fatalf("block count changed: %d", len(out))
	}
	// Both big blocks spilled with notes; small + media untouched.
	for _, i := range []int{0, 4} {
		if !strings.Contains(textOf(t, out[i]), "output guard") {
			t.Fatalf("block %d missing guard note", i)
		}
	}
	if textOf(t, out[1]) != small {
		t.Fatal("small text mangled")
	}
	if out[2].(mcp.ImageContent).Data != img {
		t.Fatal("image bytes touched — forbidden")
	}
	if out[3].(mcp.AudioContent).Data != aud {
		t.Fatal("audio bytes touched — forbidden")
	}
	// Input slice must be unchanged (spilled blocks are copies).
	if in[0].(mcp.TextContent).Text != big1 || in[4].(mcp.TextContent).Text != big2 {
		t.Fatal("Apply mutated the caller's blocks")
	}
	// Clean up the two spill files.
	for _, i := range []int{0, 4} {
		if p := spillPathFromNote(textOf(t, out[i])); p != "" {
			_ = removeSpill(p)
		}
	}
}

func TestNegativeLimitsClamped(t *testing.T) {
	// Negative limits clamp to 0 (spill everything); never panic.
	for _, lim := range []Limits{{-1, 10}, {10, -1}, {-1, -1}, {0, -1}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Apply(%+v) panicked: %v", lim, r)
				}
			}()
			out, spilled := Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}, lim)
			if !spilled {
				t.Errorf("Apply(%+v) must spill", lim)
			}
			_ = out
		}()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Apply(%+v) empty panicked: %v", lim, r)
				}
			}()
			Apply([]mcp.Content{mcp.TextContent{Type: "text", Text: ""}}, lim)
		}()
	}
	// Non-text blocks never reach head: no panic even with negative limits.
	out, spilled := Apply(
		[]mcp.Content{mcp.ImageContent{Type: "image", Data: "QUJD", MIMEType: "image/png"}},
		Limits{MaxBytes: -1, MaxLines: -1},
	)
	if spilled || out[0].(mcp.ImageContent).Data != "QUJD" {
		t.Fatal("media must pass through regardless of limits")
	}
}
