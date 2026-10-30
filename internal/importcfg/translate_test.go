package importcfg

import (
	"reflect"
	"strings"
	"testing"
)

func parseOne(t *testing.T, app, body string) RawEntry {
	t.Helper()
	m, err := Parse(app, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return m["a"]
}

func TestFlexStrListCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"string", `{"mcpServers":{"a":{"command":"uvx"}}}`, []string{"uvx"}},
		{"array", `{"mcpServers":{"a":{"command":["a","b"]}}}`, []string{"a", "b"}},
		{"emptyArray", `{"mcpServers":{"a":{"command":[]}}}`, []string{}},
		// current behavior: JSON null decodes via the string branch as "".
		{"nullBecomesEmptyString", `{"mcpServers":{"a":{"command":null}}}`, []string{""}},
		{"emptyString", `{"mcpServers":{"a":{"command":""}}}`, []string{""}},
		{"missing", `{"mcpServers":{"a":{}}}`, nil},
		{"stringPlusArgs", `{"mcpServers":{"a":{"command":"uvx","args":["srv"]}}}`, []string{"uvx", "srv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOne(t, "cursor", tc.body).Command
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("cmd = %q, want %q", got, tc.want)
			}
		})
	}
	// Numbers are a hard error for the whole file (no per-entry salvage).
	if _, err := Parse("cursor", []byte(`{"mcpServers":{"a":{"command":5}}}`)); err == nil {
		t.Fatal("numeric command must fail")
	}
	// Mixed-type arrays fail the whole file.
	if _, err := Parse("cursor", []byte(`{"mcpServers":{"a":{"command":["a",1]}}}`)); err == nil {
		t.Fatal("mixed array must fail")
	}
}

func TestOAuthOnTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		frag string
		want bool
	}{
		{"absent", `{"command":"x"}`, false},
		{"null", `{"command":"x","oauth":null}`, false},
		{"false", `{"command":"x","oauth":false}`, false},
		{"true", `{"command":"x","oauth":true}`, true},
		{"zeroNumber", `{"command":"x","oauth":0}`, true}, // current behavior: non-bool/number always on
		{"array", `{"command":"x","oauth":[]}`, true},     // current behavior
		{"emptyMap", `{"command":"x","oauth":{}}`, true},  // key present = configured
		{"map", `{"command":"x","oauth":{"type":"oauth"}}`, true},
		{"emptyString", `{"command":"x","oauth":""}`, false},
		{"falseString", `{"command":"x","oauth":"false"}`, false},
		{"falsePaddedCase", `{"command":"x","oauth":" FALSE "}`, false},
		{"noneString", `{"command":"x","oauth":"none"}`, false},
		{"disabledString", `{"command":"x","oauth":"disabled"}`, false},
		{"zeroString", `{"command":"x","oauth":"0"}`, true}, // current behavior: only false/none/disabled/"" are off
		{"deviceString", `{"command":"x","oauth":"device"}`, true},
		{"authNone", `{"command":"x","auth":"none"}`, false},
		{"authMap", `{"command":"x","auth":{"type":"oauth"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseOne(t, "cursor", `{"mcpServers":{"a":`+tc.frag+`}}`).OAuth; got != tc.want {
				t.Fatalf("oauth = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNormalizeTable(t *testing.T) {
	t.Run("envPrecedence", func(t *testing.T) {
		r := parseOne(t, "opencode", `{"mcp":{"a":{"command":["x"],"env":{"K":"env"},"environment":{"K":"environment"}}}}`)
		if r.Env["K"] != "environment" {
			t.Fatalf("environment must win: %v", r.Env)
		}
	})
	t.Run("argsAppended", func(t *testing.T) {
		r := parseOne(t, "cursor", `{"mcpServers":{"a":{"command":"uvx","args":["srv","--x"]}}}`)
		if !reflect.DeepEqual(r.Command, []string{"uvx", "srv", "--x"}) {
			t.Fatalf("cmd = %q", r.Command)
		}
	})
	for _, tc := range []struct {
		name string
		typ  string
		want string
	}{
		{"sseLower", "sse", "sse"},
		{"sseUpper", "SSE", "sse"},
		{"ssePadded", " sse ", "sse"},
		{"httpAuto", "http", ""},
		{"localAuto", "local", ""},
		{"stdioAuto", "stdio", ""},
		{"remoteAuto", "remote", ""},
		{"empty", "", ""},
	} {
		t.Run("type/"+tc.name, func(t *testing.T) {
			r := parseOne(t, "cursor", `{"mcpServers":{"a":{"command":"x","type":"`+tc.typ+`"}}}`)
			if r.Transport != tc.want {
				t.Fatalf("transport = %q, want %q", r.Transport, tc.want)
			}
		})
	}
	t.Run("timeoutNote", func(t *testing.T) {
		r := parseOne(t, "cursor", `{"mcpServers":{"a":{"command":"x","timeout":30}}}`)
		if len(r.Dropped) != 1 || !strings.Contains(r.Dropped[0], "timeout dropped") {
			t.Fatalf("dropped = %v", r.Dropped)
		}
	})
	t.Run("urlTrimmed", func(t *testing.T) {
		r := parseOne(t, "cursor", `{"mcpServers":{"a":{"url":"  https://x/mcp  "}}}`)
		if r.URL != "https://x/mcp" {
			t.Fatalf("url = %q", r.URL)
		}
	})
	t.Run("enabledPointer", func(t *testing.T) {
		r := parseOne(t, "opencode", `{"mcp":{"a":{"command":["x"],"enabled":false}}}`)
		if r.Enabled == nil || *r.Enabled {
			t.Fatalf("enabled = %+v", r.Enabled)
		}
	})
	t.Run("cwd", func(t *testing.T) {
		r := parseOne(t, "cursor", `{"mcpServers":{"a":{"command":"x","cwd":"/tmp"}}}`)
		if r.Cwd != "/tmp" {
			t.Fatalf("cwd = %q", r.Cwd)
		}
	})
}

func TestToOxypTranslators(t *testing.T) {
	t.Run("localBasic", func(t *testing.T) {
		e, notes, err := ToOxyp("cursor", RawEntry{Name: "l", Command: []string{"npx", "srv"}, Env: map[string]string{"K": "V"}, Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		if len(notes) != 0 || e.URL != "" || e.Cwd != "/tmp" || e.Env["K"] != "V" {
			t.Fatalf("local = %+v notes=%v", e, notes)
		}
		if e.Description != "Imported from cursor" {
			t.Fatalf("description = %q", e.Description)
		}
	})
	t.Run("remoteBasic", func(t *testing.T) {
		e, notes, err := ToOxyp("cursor", RawEntry{Name: "r", URL: "https://x/mcp", Headers: map[string]string{"H": "v"}, Transport: "sse"})
		if err != nil {
			t.Fatal(err)
		}
		if e.URL == "" || e.Transport != "sse" || len(e.Command) != 0 || len(notes) != 0 {
			t.Fatalf("remote = %+v notes=%v", e, notes)
		}
	})
	t.Run("remoteOAuthNote", func(t *testing.T) {
		e, notes, err := ToOxyp("cursor", RawEntry{Name: "r", URL: "https://x/mcp", OAuth: true})
		if err != nil {
			t.Fatal(err)
		}
		if e.Auth != "oauth" || len(notes) == 0 || !strings.Contains(notes[0], "oauth") {
			t.Fatalf("oauth = %+v notes=%v", e, notes)
		}
	})
	t.Run("commandAndURLKeepsURL", func(t *testing.T) {
		e, notes, err := ToOxyp("cursor", RawEntry{Name: "r", URL: "https://x/mcp", Command: []string{"x"}})
		if err != nil {
			t.Fatal(err)
		}
		if e.URL == "" || len(e.Command) != 0 {
			t.Fatalf("must keep url: %+v", e)
		}
		if len(notes) == 0 || !strings.Contains(notes[0], "both command and url") {
			t.Fatalf("notes = %v", notes)
		}
	})
	// Silent drops: remote Env/Cwd and local Headers/Transport/OAuth vanish
	// with no note. Current behavior — pinned so a fix is visible.
	t.Run("remoteDropsEnvCwdSilently", func(t *testing.T) {
		e, notes, err := ToOxyp("cursor", RawEntry{Name: "r", URL: "https://x/mcp", Env: map[string]string{"A": "b"}, Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Env) != 0 || e.Cwd != "" {
			t.Fatalf("remote env/cwd must be dropped: %+v", e)
		}
		if len(notes) != 0 {
			t.Fatalf("no note expected: %v", notes)
		}
	})
	t.Run("localDropsHeadersTransportOAuthSilently", func(t *testing.T) {
		e, notes, err := ToOxyp("cursor", RawEntry{Name: "l", Command: []string{"x"}, Headers: map[string]string{"H": "v"}, Transport: "sse", OAuth: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Headers) != 0 || e.Transport != "" || e.Auth != "" {
			t.Fatalf("local extras must be dropped: %+v", e)
		}
		if len(notes) != 0 {
			t.Fatalf("no note expected: %v", notes)
		}
	})
	t.Run("neitherErrors", func(t *testing.T) {
		if _, _, err := ToOxyp("cursor", RawEntry{Name: "n"}); err == nil || !strings.Contains(err.Error(), "neither command nor url") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("droppedPreservedNoMutation", func(t *testing.T) {
		raw := RawEntry{Name: "r", Command: []string{"x"}, Dropped: []string{"d1"}}
		_, notes, err := ToOxyp("cursor", raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(notes, []string{"d1"}) {
			t.Fatalf("notes = %v", notes)
		}
		if !reflect.DeepEqual(raw.Dropped, []string{"d1"}) {
			t.Fatalf("input mutated: %v", raw.Dropped)
		}
	})
	t.Run("enabledPassthrough", func(t *testing.T) {
		e, _, err := ToOxyp("cursor", RawEntry{Name: "l", Command: []string{"x"}, Enabled: new(false)})
		if err != nil {
			t.Fatal(err)
		}
		if e.Enabled == nil || *e.Enabled {
			t.Fatalf("enabled not passed: %+v", e.Enabled)
		}
	})
}
