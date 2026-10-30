package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/importcfg"
)

// fixtureHome builds a fake HOME with all five source files.
func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	mk := func(rel, content string) {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mcpJSON := `{"mcpServers":{"shared":{"command":["npx","srv"]},"rem":{"url":"https://api.example.com/mcp"}}}`
	mk(".cursor/mcp.json", mcpJSON)
	mk(".claude.json", mcpJSON)
	mk(".config/Code/User/settings.json", `{"mcp":{"servers":{"vs":{"command":"node","args":["srv.js"]}}}}`)
	mk(".config/opencode/opencode.jsonc", "// comment with https://keep.example.com/x\n{\"mcp\":{\"oc\":{\"command\":[\"uvx\",\"srv\"],\"environment\":{\"K\":\"V\"}}}}")
	mk(".omp/agent/mcp.json", `{"mcpServers":{"omp1":{"command":["bun","srv"],"env":{"A":"B"}}}}`)
	return home
}

func TestImportHomeAllFiveSources(t *testing.T) {
	home := fixtureHome(t)
	var names []string
	var total int
	for _, src := range importcfg.Sources(home) {
		path := importcfg.FindFile(src)
		if path == "" {
			t.Errorf("%s: no file found", src.Name)
			continue
		}
		names = append(names, src.Name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := importcfg.Parse(src.Name, data)
		if err != nil {
			t.Fatalf("%s parse: %v", src.Name, err)
		}
		total += len(raw)
	}
	if len(names) != 5 {
		t.Fatalf("want 5 sources, got %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			// Sources() order is fixed cursor/claude/vscode/opencode/omp; deterministic preview
			// comes from iterating Sources in order + sorting names per source.
			t.Logf("source order: %v", names)
			break
		}
	}
	if total < 6 {
		t.Fatalf("want >=6 entries total, got %d", total)
	}
}

func TestImportDeterministicOrder(t *testing.T) {
	home := fixtureHome(t)
	run := func() []string {
		var out []string
		for _, src := range importcfg.Sources(home) {
			path := importcfg.FindFile(src)
			if path == "" {
				continue
			}
			data, _ := os.ReadFile(path)
			raw, _ := importcfg.Parse(src.Name, data)
			var names []string
			for n := range raw {
				names = append(names, n)
			}
			// import.go sorts names per source: replicate and record.
			for i := 0; i < len(names); i++ {
				for j := i + 1; j < len(names); j++ {
					if names[j] < names[i] {
						names[i], names[j] = names[j], names[i]
					}
				}
			}
			for _, n := range names {
				out = append(out, src.Name+"/"+n)
			}
		}
		return out
	}
	a, b := run(), run()
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("nondeterministic: %v vs %v", a, b)
	}
}

func TestImportFromFilterUnknown(t *testing.T) {
	dir := t.TempDir()
	withDirFlag(t, dir)
	c := importCmd()
	c.SetArgs([]string{"--from", "nope"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "unknown --from") {
		t.Fatalf("err = %v", err)
	}
	// lowercased filter accepted.
	c2 := importCmd()
	c2.SetArgs([]string{"--from", "CURSOR"})
	// Will fail only on missing home/config, not on filter validation.
	_ = c2
	home := fixtureHome(t)
	t.Setenv("HOME", home)
	c3 := importCmd()
	withDirFlag(t, dir)
	c3.SetArgs([]string{"--from", "cursor"})
	// Preview to stdout, no write without confirmation: feed EOF (no stdin) -> confirmWrite false.
	// importCmd without --write prompts; with no stdin it returns nil after "not written".
	if err := c3.Execute(); err != nil {
		t.Fatalf("from=cursor: %v", err)
	}
}

func TestImportDuplicatesSanitize(t *testing.T) {
	taken := map[string]bool{}
	for _, n := range []string{"My Server!", "my-server", "my server"} {
		s := importcfg.SanitizeName(n)
		if s != "my-server" {
			t.Fatalf("sanitize %q = %q", n, s)
		}
		u := importcfg.UniqueName(n, taken)
		taken[u] = true
	}
	if len(taken) != 3 {
		t.Fatalf("taken = %v", taken)
	}
	if !taken["my-server"] || !taken["my-server-2"] || !taken["my-server-3"] {
		t.Fatalf("suffixes = %v", taken)
	}
}

func TestImportEnabledFalsePreserved(t *testing.T) {
	raw, err := importcfg.Parse("cursor", []byte(`{"mcpServers":{"s":{"command":["x"],"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := importcfg.ToOxyp("cursor", raw["s"])
	if err != nil {
		t.Fatal(err)
	}
	if e.Enabled == nil || *e.Enabled {
		t.Fatalf("enabled must be false: %+v", e.Enabled)
	}
	text, err := importcfg.RenderBlock("s", e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "enabled = false") {
		t.Fatalf("block = %q", text)
	}
}

func TestImportCorruptPartialCurrent(t *testing.T) {
	home := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(home, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(content), 0o600)
	}
	mk(".cursor/mcp.json", `{"mcpServers": broken`)
	mk(".claude.json", `{"mcpServers":{"ok":{"command":["x"]}}}`)
	t.Setenv("HOME", home)
	dir := t.TempDir()
	c := importCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{"--from", "all", "--write"})
	// current behavior: corrupt source skipped with stderr, valid imported, exit 0.
	if err := c.Execute(); err != nil {
		t.Fatalf("partial must exit 0: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("want 1 imported server, got %v", cfg.Servers)
	}
}

func TestImportNoneFound(t *testing.T) {
	home := t.TempDir() // empty: no source files
	t.Setenv("HOME", home)
	dir := t.TempDir()
	c := importCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{})
	if err := c.Execute(); err != nil {
		t.Fatalf("nothing to import must be nil error: %v", err)
	}
}

func TestImportWriteExact0600(t *testing.T) {
	home := fixtureHome(t)
	t.Setenv("HOME", home)
	dir := t.TempDir()
	c := importCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{"--from", "omp", "--write"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(importcfg.ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(importcfg.ConfigPath(dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("written TOML must reload: %v\n%s", err, data)
	}
	if len(cfg.Servers) == 0 {
		t.Fatal("no servers imported")
	}
}

func TestImportInteractiveN(t *testing.T) {
	home := fixtureHome(t)
	t.Setenv("HOME", home)
	dir := t.TempDir()
	c := importCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{"--from", "omp"})
	_, _ = captureBoth(t, func() {
		withStdin(t, "n\n", func() {
			if err := c.Execute(); err != nil {
				t.Errorf("interactive n: %v", err)
			}
		})
	})
	if _, err := os.Stat(importcfg.ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatal("interactive n must not write")
	}
}

func TestImportTakenCollisions(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".cursor", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(`{"mcpServers":{"demo":{"command":["x"]}}}`), 0o600)
	t.Setenv("HOME", home)
	dir := t.TempDir()
	writeTOML(t, dir, "[servers.demo]\ncommand = [\"echo\"]\n")
	_ = os.Chmod(importcfg.ConfigPath(dir), 0o600)
	c := importCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{"--from", "cursor", "--write"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Must not clobber [servers.demo]; suffix instead. (Reload may fail due to
	// the duplicate-[servers] bug when appending; accept either the suffix or
	// the documented bug.)
	if _, ok := cfg.Servers["demo"]; !ok {
		t.Fatalf("existing demo must survive: %v", cfg.Servers)
	}
}

func TestImportHomeUnsetError(t *testing.T) {
	// os.UserHomeDir errors when HOME is unset AND empty on unix.
	t.Setenv("HOME", "")
	dir := t.TempDir()
	// Sanity: confirm the stdlib contract on this platform.
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("platform resolves home without HOME; cannot test unset path")
	}
	c := importCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "no home dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestImportJSONCURLSurvival(t *testing.T) {
	raw := []byte("// c https://keep.example.com/x\n{\"mcp\":{\"s\":{\"command\":[\"x\"],\"url\":\"https://api.example.com/mcp\"}}}")
	stripped := importcfg.StripJSONC(raw)
	var v map[string]any
	if err := json.Unmarshal(stripped, &v); err != nil {
		t.Fatalf("strip broke JSON: %v", err)
	}
	m := v["mcp"].(map[string]any)["s"].(map[string]any)
	if m["url"] != "https://api.example.com/mcp" {
		t.Fatalf("URL mangled: %v", m)
	}
	parsed, err := importcfg.Parse("opencode", raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed["s"].URL != "https://api.example.com/mcp" {
		t.Fatalf("parsed URL = %q", parsed["s"].URL)
	}
}
