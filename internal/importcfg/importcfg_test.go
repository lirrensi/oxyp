package importcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
)

func TestFixtures(t *testing.T) {
	dir := "testdata"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".json" && filepath.Ext(name) != ".jsonc" {
			continue
		}
		app := strings.SplitN(strings.TrimSuffix(name, filepath.Ext(name)), "-", 2)[0]
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := Parse(app, data)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			if len(raw) == 0 {
				t.Fatalf("%s: no entries parsed", name)
			}
			for n, r := range raw {
				entry, notes, err := ToOxyp(app, r)
				if err != nil {
					t.Fatalf("%s/%s: %v", name, n, err)
				}
				text, err := RenderBlock(n, entry, notes)
				if err != nil {
					t.Fatalf("%s/%s render: %v", name, n, err)
				}
				if !strings.Contains(text, "[servers."+n+"]") {
					t.Fatalf("%s/%s: missing table header in:\n%s", name, n, text)
				}
			}
		})
	}
}

func TestLocalVsRemoteShapes(t *testing.T) {
	raw, err := Parse("omp", []byte(`{"mcpServers": {
		"loc": {"type": "stdio", "command": "uvx", "args": ["srv"], "env": {"K": "V"}},
		"rem": {"type": "http", "url": "https://api.example.com/mcp", "headers": {"Authorization": "Bearer X"}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	loc, _, err := ToOxyp("omp", raw["loc"])
	if err != nil {
		t.Fatal(err)
	}
	if len(loc.Command) != 2 || loc.Command[0] != "uvx" || loc.URL != "" || loc.Env["K"] != "V" {
		t.Fatalf("local mistranslated: %+v", loc)
	}
	rem, _, err := ToOxyp("omp", raw["rem"])
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL == "" || len(rem.Command) != 0 {
		t.Fatalf("remote mistranslated: %+v", rem)
	}
}

func TestOpenCodeShapes(t *testing.T) {
	raw, err := Parse("opencode", []byte(`{
		// comment must be stripped, but "https://" strings must survive
		"mcp": {
			"loc": {"type": "local", "command": ["npx", "srv"], "environment": {"K": "V"}, /* block */ "enabled": false},
			"rem": {"type": "remote", "url": "https://x.example.com/mcp", "headers": {}, "oauth": {}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	loc, _, err := ToOxyp("opencode", raw["loc"])
	if err != nil {
		t.Fatal(err)
	}
	if len(loc.Command) != 2 || loc.Enabled == nil || *loc.Enabled {
		t.Fatalf("opencode local mistranslated: %+v", loc)
	}
	rem, notes, err := ToOxyp("opencode", raw["rem"])
	if err != nil {
		t.Fatal(err)
	}
	if rem.Auth != "oauth" || len(notes) == 0 {
		t.Fatalf("opencode oauth mistranslated: %+v notes=%v", rem, notes)
	}
}
func mustEntry(t *testing.T, app string, r RawEntry) config.ServerEntry {
	t.Helper()
	e, _, err := ToOxyp(app, r)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestVSCodeSettingsShape(t *testing.T) {
	raw, err := Parse("vscode", []byte(`{"mcp": {"servers": {
		"s": {"command": "node", "args": ["srv.js"]}
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["s"]; !ok {
		t.Fatalf("vscode settings shape not parsed: %v", raw)
	}
}

func TestNameHelpers(t *testing.T) {
	n, err := NameFromURL("https://company.glean.com/mcp")
	if err != nil || n != "company-glean-com" {
		t.Fatalf("NameFromURL = %q, %v", n, err)
	}
	if _, err := NameFromURL("not a url"); err == nil {
		t.Fatal("NameFromURL must reject bad urls")
	}
	taken := map[string]bool{"srv": true, "srv-2": true}
	if got := UniqueName("SRV", taken); got != "srv-3" {
		t.Fatalf("UniqueName = %q", got)
	}
	if SanitizeName("My Server_2!") != "my-server-2" {
		t.Fatalf("SanitizeName = %q", SanitizeName("My Server_2!"))
	}
}

func TestAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	text, err := RenderBlock("s", mustEntry(t, "cursor", RawEntry{Command: []string{"npx", "srv"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendBlocks(dir, text); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendBlocks(dir, text); err != nil {
		t.Fatal(err)
	}
}

func TestVSCodeTopLevelServersShape(t *testing.T) {
	raw, err := Parse("vscode", []byte(`{"servers": {
		"s": {"command": "node", "args": ["srv.js"]}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["s"]; !ok {
		t.Fatalf("vscode top-level servers shape not parsed: %v", raw)
	}
}

func TestAppendPerms0600(t *testing.T) {
	dir := t.TempDir()
	text, err := RenderBlock("s", mustEntry(t, "cursor", RawEntry{Command: []string{"npx", "srv"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendBlocks(dir, text); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("new oxyp.toml mode = %o, want 600", st.Mode().Perm())
	}
	// Existing world-readable file gets tightened with a printed note.
	if err := os.Chmod(ConfigPath(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	note, err := AppendBlocks(dir, text)
	if err != nil {
		t.Fatal(err)
	}
	if note == "" {
		t.Fatal("want chmod note for group/world-readable file")
	}
	st, _ = os.Stat(ConfigPath(dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("tightened mode = %o, want 600", st.Mode().Perm())
	}
}
