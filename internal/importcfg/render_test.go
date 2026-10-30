package importcfg

import (
	"strings"
	"testing"

	"oxyp/internal/config"
)

func TestRenderBlockGolden(t *testing.T) {
	e, notes, err := ToOxyp("cursor", RawEntry{Name: "s", Command: []string{"npx", "srv"}, Env: map[string]string{"K": "V"}, Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := RenderBlock("s", e, notes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[servers.s]",
		`command = ["npx", "srv"]`,
		`description = "Imported from cursor"`,
		`env = { "K" = "V" }`,
		`cwd = "/tmp"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestRenderBlockAllFields(t *testing.T) {
	off := false
	e := config.ServerEntry{
		Description: "d",
		Command:     []string{"x"},
		URL:         "https://x/mcp",
		Transport:   "sse",
		Headers:     map[string]string{"H": "v"},
		Env:         map[string]string{"K": "V"},
		Cwd:         "/tmp",
		Enabled:     &off,
		Auth:        "oauth",
	}
	text, err := RenderBlock("n", e, []string{"n1", "n2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "# note: n1\n# note: n2\n") {
		t.Fatalf("notes must lead:\n%s", text)
	}
	for _, want := range []string{`auth = "oauth"`, `enabled = false`, `transport = "sse"`, `headers = { "H" = "v" }`, `env = { "K" = "V" }`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestRenderBlockNameQuoting(t *testing.T) {
	e := mustEntry(t, "cursor", RawEntry{Command: []string{"x"}})
	text, err := RenderBlock("my srv.d", e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `[servers."my srv.d"]`) {
		t.Fatalf("name must be quoted:\n%s", text)
	}
}

func TestRenderBlockEmptyEntry(t *testing.T) {
	text, err := RenderBlock("e", config.ServerEntry{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[servers.e]") {
		t.Fatalf("header missing:\n%s", text)
	}
}

func TestRenderBlockNoteInjectionSanitized(t *testing.T) {
	// Notes come from foreign configs: newlines are flattened so a note
	// can never inject TOML tables.
	e := mustEntry(t, "cursor", RawEntry{Command: []string{"x"}})
	text, err := RenderBlock("s", e, []string{"line1\n[servers.evil]\ncommand=[\"x\"]"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "[servers.") && line != "[servers.s]" {
			t.Fatalf("note injected a table:\n%s", text)
		}
	}
	if !strings.Contains(text, "# note: line1 [servers.evil]") {
		t.Fatalf("note must survive flattened:\n%s", text)
	}
}

func TestRenderBlockConfigLoadRoundTrip(t *testing.T) {
	e, notes, err := ToOxyp("cursor", RawEntry{Name: "s", Command: []string{"npx", "srv"}, Env: map[string]string{"K": "V"}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := RenderBlock("s", e, notes)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := AppendBlocks(dir, text); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Servers["s"]
	if got.ConfigError != "" {
		t.Fatalf("ConfigError = %q", got.ConfigError)
	}
	if len(got.Command) != 2 || got.Env["K"] != "V" || got.Description != "Imported from cursor" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestRenderAllExact(t *testing.T) {
	// current behavior: empty input yields a lone newline, not "".
	if got := RenderAll(nil); got != "\n" {
		t.Fatalf("empty = %q", got)
	}
	b1, _ := RenderBlock("a", mustEntry(t, "cursor", RawEntry{Command: []string{"x"}}), nil)
	if got := RenderAll([]NamedBlock{{Name: "a", Text: b1}}); got != b1 {
		t.Fatalf("single mismatch:\n%q\nvs\n%q", got, b1)
	}
	b2, _ := RenderBlock("b", mustEntry(t, "cursor", RawEntry{Command: []string{"y"}}), nil)
	got := RenderAll([]NamedBlock{{Name: "a", Text: b1}, {Name: "b", Text: b2}})
	if got != b1+"\n"+b2 {
		t.Fatalf("two-block join wrong:\n%q", got)
	}
	// Multi-block output must be valid TOML (no repeated [servers] header).
	// Blocks lacking trailing newlines get exactly one "\n" separator —
	// not a blank line. Current behavior pinned.
	if got := RenderAll([]NamedBlock{{Name: "a", Text: "[x]"}, {Name: "b", Text: "[y]\n"}}); got != "[x]\n[y]\n" {
		t.Fatalf("bare join = %q", got)
	}
}
