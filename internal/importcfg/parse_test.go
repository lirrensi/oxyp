package importcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripJSONCTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"a": 1}`, `{"a": 1}`},
		{"lineComment", "{\"a\": 1 // hi\n}", "{\"a\": 1 \n}"},
		{"lineCommentEOF", `{"a": 1} // hi`, `{"a": 1} `},
		{"blockComment", `{"a": /* x */ 1}`, `{"a":  1}`},
		{"blockMultiline", "{\"a\": /* x\ny */ 1}", "{\"a\":  1}"},
		{"urlSurvives", `{"u": "https://x/y"}`, `{"u": "https://x/y"}`},
		{"commentInURLSurvives", `{"u": "https://x//y"}`, `{"u": "https://x//y"}`},
		{"escapedQuote", `{"a": "x\"//y"}`, `{"a": "x\"//y"}`},
		{"blockBetweenTokens", `{"a":1}/*x*/{"b":2}`, `{"a":1}{"b":2}`},
		{"slashAlone", `{"a": "b/c"}`, `{"a": "b/c"}`},
		// current behavior: unterminated block swallows the rest silently.
		{"unterminatedBlock", `{"a": 1 /* foo`, `{"a": 1 `},
		{"unterminatedLine", "{\"a\": 1 // foo", "{\"a\": 1 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(StripJSONC([]byte(tc.in))); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestParseVSCodeJSONCCurrent(t *testing.T) {
	// current behavior: ParseVSCode uses strict encoding/json, so commented
	// settings.json FAILS. Pinned so a future JSONC fix visibly changes it.
	_, err := Parse("vscode", []byte("// comment\n{\"mcp\": {\"servers\": {\"s\": {\"command\": \"x\"}}}}"))
	if err == nil {
		t.Fatal("commented vscode must currently fail (strict JSON)")
	}
	// current behavior: trailing commas fail everywhere.
	for _, tc := range []struct {
		name string
		app  string
		body string
	}{
		{"vscodeTrailing", "vscode", "{\"mcp\": {\"servers\": {\"s\": {\"command\": \"x\",}}}}"},
		{"opencodeTrailing", "opencode", "{\"mcp\": {\"s\": {\"command\": \"x\",}}}"},
		{"cursorTrailing", "cursor", "{\"mcpServers\": {\"s\": {\"command\": \"x\",}}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.app, []byte(tc.body)); err == nil {
				t.Fatalf("%s trailing comma must currently fail", tc.name)
			}
		})
	}
}

func TestParseDispatchAndEmptyShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		app     string
		body    string
		wantN   int
		wantErr string
	}{
		{"unknownApp", "bogus", `{}`, 0, "unknown source"},
		{"caseMismatch", "Cursor", `{"mcpServers":{}}`, 0, "unknown source"},
		{"emptyString", "", `{}`, 0, "unknown source"},
		{"cursorEmpty", "cursor", `{"mcpServers":{}}`, 0, ""},
		{"cursorMissing", "cursor", `{}`, 0, "no mcpServers key"},
		{"cursorNullMap", "cursor", `{"mcpServers":null}`, 0, "no mcpServers key"},
		{"opencodeEmpty", "opencode", `{"mcp":{}}`, 0, ""},
		{"opencodeMissing", "opencode", `{"other":{}}`, 0, "no mcp key"},
		{"opencodeNull", "opencode", `{"mcp":null}`, 0, "no mcp key"},
		// current behavior: {"mcp":{"servers":{}}} falls through to
		// ParseMCPServerMap and errors instead of returning empty.
		{"vscodeEmptyMCP", "vscode", `{"mcp":{}}`, 0, "no mcpServers key"},
		{"vscodeEmptyServers", "vscode", `{"mcp":{"servers":{}}}`, 0, "no mcpServers key"},
		{"vscodeTopEmpty", "vscode", `{"mcpServers":{}}`, 0, ""},
		{"vscodeBare", "vscode", `{"mcpServers":{"s":{"command":"x"}}}`, 1, ""},
		{"invalidJSON", "vscode", `not json`, 0, "invalid character"},
		{"commentedStrict", "vscode", "// c\n{}", 0, "invalid character"},
		{"oneBadEntryFailsAll", "cursor", `{"mcpServers":{"ok":{"command":"x"},"bad":{"command":5}}}`, 0, "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.app, []byte(tc.body))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(m) != tc.wantN {
				t.Fatalf("n = %d, want %d", len(m), tc.wantN)
			}
		})
	}
}

func TestSourcesFindFile(t *testing.T) {
	home := t.TempDir()
	srcs := Sources(home)
	if len(srcs) != 5 {
		t.Fatalf("want 5 sources, got %d", len(srcs))
	}
	wantNames := []string{"cursor", "claude", "vscode", "opencode", "omp"}
	for i, n := range wantNames {
		if srcs[i].Name != n {
			t.Fatalf("source[%d] = %q, want %q", i, srcs[i].Name, n)
		}
	}
	if got := len(srcs[2].Files); got != 4 {
		t.Fatalf("vscode must have 4 candidates, got %d", got)
	}
	// Nothing exists yet.
	for _, s := range srcs {
		if got := FindFile(s); got != "" {
			t.Fatalf("FindFile(%s) = %q, want empty", s.Name, got)
		}
	}
	// First hit wins: create the second cursor candidate only, then the first.
	writeProbe(t, home, ".config/Cursor/mcp.json", `{}`)
	if got := FindFile(srcs[0]); !strings.HasSuffix(got, ".config/Cursor/mcp.json") {
		t.Fatalf("second candidate = %q", got)
	}
	writeProbe(t, home, ".cursor/mcp.json", `{}`)
	if got := FindFile(srcs[0]); !strings.HasSuffix(got, ".cursor/mcp.json") {
		t.Fatalf("first must win = %q", got)
	}
	// Directories are ignored.
	dirSrc := Source{Name: "x", Files: []string{home, home + "/.cursor/mcp.json"}}
	if got := FindFile(dirSrc); got != home+"/.cursor/mcp.json" {
		t.Fatalf("directory must be skipped: %q", got)
	}
	// Missing everything → "".
	if got := FindFile(Source{Name: "x", Files: []string{home + "/nope"}}); got != "" {
		t.Fatalf("missing = %q", got)
	}
}

func writeProbe(t *testing.T, home, rel, body string) {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
