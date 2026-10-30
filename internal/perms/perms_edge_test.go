package perms

import (
	"strings"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
)

func TestResolveNilEntryAllows(t *testing.T) {
	d := Resolve(nil, "anything")
	if d.Action != "allow" {
		t.Fatalf("nil entry should allow: %+v", d)
	}
	if d.Message != "" {
		t.Fatalf("allow decision must have empty message, got %q", d.Message)
	}
}

func TestResolveDenyOnly(t *testing.T) {
	srv := entry(&config.ToolsPolicy{Deny: []string{"secret_*"}})
	if d := Resolve(srv, "secret_x"); d.Action != "deny" {
		t.Fatalf("deny-only should deny: %+v", d)
	}
	if d := Resolve(srv, "public"); d.Action != "allow" {
		t.Fatalf("deny-only non-match should allow: %+v", d)
	}
}

func TestResolveDenyMessageExact(t *testing.T) {
	srv := entry(&config.ToolsPolicy{Deny: []string{"delete_*"}})
	d := Resolve(srv, "delete_x")
	if d.Action != "deny" {
		t.Fatalf("expected deny: %+v", d)
	}
	want := `matched deny pattern "delete_*" on server "s"`
	if d.Message != want {
		t.Fatalf("deny message = %q, want %q", d.Message, want)
	}
}

func TestResolveAllowMissMessageExact(t *testing.T) {
	srv := entry(&config.ToolsPolicy{Allow: []string{"get_*"}})
	d := Resolve(srv, "other")
	if d.Action != "deny" {
		t.Fatalf("expected deny: %+v", d)
	}
	want := `not matched by any allow pattern on server "s"`
	if d.Message != want {
		t.Fatalf("allow-miss message = %q, want %q", d.Message, want)
	}
}

func TestResolveAllowHitEmptyMessage(t *testing.T) {
	srv := entry(&config.ToolsPolicy{Allow: []string{"get_*"}})
	d := Resolve(srv, "get_x")
	if d.Action != "allow" || d.Message != "" {
		t.Fatalf("allow hit must have empty message: %+v", d)
	}
}

func TestGlobLiteralRegexMetachars(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"a+b", "a+b", true},
		{"a+b", "aaab", false},
		{"a(b)", "a(b)", true},
		{"a(b)", "ab", false},
		{"a[bc]", "a[bc]", true},
		{"a[bc]", "ab", false},
		{"a^b$c", "a^b$c", true},
		{"a^b$c", "aXbYc", false},
		{"a|b", "a|b", true},
		{"a|b", "a", false},
		{"a\\b", `a\b`, true},
		{"a\\b", "ab", false},
		{"(foo)+", "(foo)+", true},
		{"(foo)+", "foofoo", false},
	}
	for _, c := range cases {
		if got := matches(c.pattern, c.value); got != c.want {
			t.Errorf("matches(%q,%q) = %v, want %v", c.pattern, c.value, got, c.want)
		}
	}
}

func TestGlobRuneSemantics(t *testing.T) {
	// ? matches one rune, not one byte: é is 2 bytes in UTF-8.
	if !matches("?", "é") {
		t.Error("? should match a single multi-byte rune")
	}
	// * matches across newlines (.* without (?s)? no — verify current behavior).
	got := matches("*", "a\nb")
	t.Logf("current behavior: matches(\"*\", \"a\\nb\") = %v", got)
	// ? does NOT match newline? . in Go regexp matches any char except newline.
	if matches("a?b", "a\nb") {
		t.Error("current behavior check: ? matched newline; document if this changes")
	}
}

func TestGlobCaseInsensitive(t *testing.T) {
	for _, tc := range []struct{ pattern, value string }{
		{"DELETE_*", "delete_x"},
		{"delete_*", "DELETE_X"},
		{"Get_?", "GET_X"},
		{"Äpfel", "äpfel"},
	} {
		if !matches(tc.pattern, tc.value) {
			t.Errorf("matches(%q,%q) = false, want true (case-insensitive)", tc.pattern, tc.value)
		}
	}
}

func TestResolveEmptyTool(t *testing.T) {
	allowAll := entry(&config.ToolsPolicy{})
	if d := Resolve(allowAll, ""); d.Action != "allow" {
		t.Errorf("empty tool with allow-all should allow: %+v", d)
	}
	star := entry(&config.ToolsPolicy{Allow: []string{"*"}, Deny: []string{"x"}})
	if d := Resolve(star, ""); d.Action != "allow" {
		t.Errorf("empty tool vs * should allow: %+v", d)
	}
	q := entry(&config.ToolsPolicy{Allow: []string{"?"}})
	d := Resolve(q, "")
	if d.Action != "deny" {
		t.Errorf("empty tool vs ? should deny: %+v", d)
	}
	exact := entry(&config.ToolsPolicy{Deny: []string{""}})
	if d := Resolve(exact, ""); d.Action != "deny" {
		t.Errorf("empty deny pattern should match empty tool: %+v", d)
	}
}

func TestVisibleNilAndEmptyAndOrder(t *testing.T) {
	// nil entry allows all, preserves order.
	tools := []string{"b", "a", "c"}
	got := Visible(nil, tools)
	if len(got) != 3 || got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("Visible(nil) = %v, want order preserved", got)
	}
	// nil slice → empty (non-nil?) document current behavior.
	got = Visible(nil, nil)
	t.Logf("current behavior: Visible(nil, nil) = %#v (nil=%v)", got, got == nil)
	if len(got) != 0 {
		t.Fatalf("Visible(nil, nil) should be empty, got %v", got)
	}
	// nil policy entry.
	got = Visible(entry(nil), []string{"x", "y"})
	if len(got) != 2 {
		t.Fatalf("Visible(nil policy) = %v", got)
	}
	// no-alias: mutating result must not clobber input beyond shared prefix.
	in := []string{"keep", "drop_x", "keep2"}
	srv := entry(&config.ToolsPolicy{Deny: []string{"drop_*"}})
	before := append([]string(nil), in...)
	out := Visible(srv, in)
	if len(out) != 2 || out[0] != "keep" || out[1] != "keep2" {
		t.Fatalf("Visible = %v", out)
	}
	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("Visible mutated input: was %v now %v", before, in)
		}
	}
}

func TestVisibleNoAlias(t *testing.T) {
	in := []string{"a", "b"}
	srv := entry(&config.ToolsPolicy{Deny: []string{"zzz"}})
	out := Visible(srv, in)
	out[0] = "MUT"
	if in[0] != "a" {
		t.Fatalf("Visible aliases input backing array: in=%v", in)
	}
}

func TestManyStarsPerf(t *testing.T) {
	// Collapsed ** must not blow up; document timing.
	pat := strings.Repeat("*", 1000) + "foo"
	start := time.Now()
	if !matches(pat, "xxfoo") {
		t.Fatal("many-star pattern should match")
	}
	if !matches(pat, strings.Repeat("x", 10000)) == false {
		// non-match; just measuring
	}
	el := time.Since(start)
	t.Logf("many-star match took %v", el)
	if el > 5*time.Second {
		t.Fatalf("glob match too slow: %v", el)
	}
}
