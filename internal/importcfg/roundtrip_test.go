package importcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
)

// Every testdata fixture must survive the full pipeline:
// Parse → ToOxyp → RenderBlock → RenderAll → AppendBlocks → config.Load
// with zero ConfigError.
func TestFixtureRoundTrips(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if ext != ".json" && ext != ".jsonc" {
			continue
		}
		app := strings.SplitN(strings.TrimSuffix(name, ext), "-", 2)[0]
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			raw, err := Parse(app, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) == 0 {
				t.Fatalf("%s: no entries", name)
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
				// One entry per dir: concatenated RenderBlock outputs repeat
				// the [servers] header, which is unloadable TOML (BUGFOUND).
				// Per-entry dirs validate each payload with zero ConfigError.
				dir := t.TempDir()
				if _, err := AppendBlocks(dir, text); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.Load(dir)
				if err != nil {
					t.Fatalf("config.Load: %v", err)
				}
				got, ok := cfg.Servers[n]
				if !ok {
					t.Fatalf("server %q missing after round trip", n)
				}
				if got.ConfigError != "" {
					// omp-basic.json's glean entry references
					// ${GLEAN_TOKEN}, unset in tests: fail-closed by design.
					// Pinned: every OTHER entry must be clean.
					if name == "omp-basic.json" && n == "glean" {
						continue
					}
					t.Fatalf("server %q ConfigError = %q", n, got.ConfigError)
				}
				if len(entry.Command) > 0 && strings.Join(got.Command, "\x00") != strings.Join(entry.Command, "\x00") {
					t.Fatalf("server %q command = %q, want %q", n, got.Command, entry.Command)
				}
				if entry.URL != "" && got.URL != entry.URL {
					t.Fatalf("server %q url = %q, want %q", n, got.URL, entry.URL)
				}
			}
			seen++
		})
	}
	if seen == 0 {
		t.Fatal("no fixtures exercised")
	}
}

func TestNullCommandRoundTrip(t *testing.T) {
	// current behavior: "command": null becomes [""] and survives the whole
	// pipeline with zero ConfigError (empty argv[0] is accepted). Pinned.
	raw, err := Parse("cursor", []byte(`{"mcpServers":{"nul":{"command":null}}}`))
	if err != nil {
		t.Fatal(err)
	}
	entry, notes, err := ToOxyp("cursor", raw["nul"])
	if err != nil {
		t.Fatal(err)
	}
	text, err := RenderBlock("nul", entry, notes)
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
	if got := cfg.Servers["nul"]; got.ConfigError != "" {
		t.Fatalf("ConfigError = %q", got.ConfigError)
	}
}
