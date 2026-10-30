package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestReloadUnchangedKeepsCaches(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	m := d.servers["s"]
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "cached"}}
	m.toolsCachedAt = time.Now()
	m.toolsMu.Unlock()
	m.resMu.Lock()
	m.res = []mcp.Resource{{URI: "r:1", Name: "r"}}
	m.prompts = []mcp.Prompt{{Name: "p"}}
	m.instructions = "instr"
	m.resMu.Unlock()
	m.setHealth(&Health{Healthy: true})

	added, removed, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}})
	if len(added) != 0 || len(removed) != 0 || len(modified) != 0 {
		t.Fatalf("no-op reload: +%v -%v ~%v", added, removed, modified)
	}
	m.toolsMu.RLock()
	kept := len(m.tools) == 1 && m.tools[0].Name == "cached"
	m.toolsMu.RUnlock()
	if !kept {
		t.Fatal("unchanged reload must keep tool cache")
	}
	m.resMu.RLock()
	keptRes := len(m.res) == 1 && len(m.prompts) == 1 && m.instructions == "instr"
	m.resMu.RUnlock()
	if !keptRes {
		t.Fatal("unchanged reload must keep resources/prompts/instructions")
	}
	if h := d.HealthOf("s"); h == nil || !h.Healthy {
		t.Fatalf("unchanged reload must keep health: %+v", h)
	}
}

func TestReloadDisablesRemovesEntry(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"keep": {Command: []string{"true"}},
		"drop": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	f := false
	added, removed, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"keep": {Command: []string{"true"}},
		"drop": {Command: []string{"true"}, Enabled: &f},
	}})
	if len(added) != 0 || len(modified) != 0 {
		t.Fatalf("want only removal, got +%v ~%v", added, modified)
	}
	if len(removed) != 1 || removed[0] != "drop" {
		t.Fatalf("removed = %v", removed)
	}
	if d.Entry("drop") != nil {
		t.Fatal("disabled entry must be removed")
	}
	if d.Entry("keep") == nil {
		t.Fatal("kept entry must survive")
	}
}

func TestReloadConfigErrorHealth(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	added, _, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {ConfigError: "bad url"},
		"n": {ConfigError: "also bad"},
	}})
	if len(added) != 1 || added[0] != "n" {
		t.Fatalf("added = %v", added)
	}
	if len(modified) != 1 || modified[0] != "s" {
		t.Fatalf("modified = %v", modified)
	}
	for _, name := range []string{"s", "n"} {
		h := d.HealthOf(name)
		if h == nil || h.Healthy || h.Error != "config: "+d.Entry(name).ConfigError {
			t.Fatalf("%s: want config health, got %+v", name, h)
		}
		if d.IsConnected(name) {
			t.Fatalf("%s: config-error server must show disconnected", name)
		}
	}
	// Fix clears the error and the health.
	_, _, modified = d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
		"n": {Command: []string{"true"}},
	}})
	if len(modified) != 2 {
		t.Fatalf("fix should modify both: %v", modified)
	}
	for _, name := range []string{"s", "n"} {
		if h := d.HealthOf(name); h != nil {
			t.Fatalf("%s: fixed config should clear health, got %+v", name, h)
		}
	}
}

func TestReloadModifiedClearsCaches(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	m := d.servers["s"]
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "old"}}
	m.toolsMu.Unlock()
	m.resMu.Lock()
	m.res = []mcp.Resource{{URI: "r:1"}}
	m.prompts = []mcp.Prompt{{Name: "p"}}
	m.instructions = "old-instr"
	m.resMu.Unlock()
	m.setHealth(&Health{Healthy: true})

	_, _, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"false"}},
	}})
	if len(modified) != 1 {
		t.Fatalf("modified = %v", modified)
	}
	m.toolsMu.RLock()
	cleared := m.tools == nil
	m.toolsMu.RUnlock()
	if !cleared {
		t.Fatal("modified reload must clear tool cache")
	}
	m.resMu.RLock()
	clearedRes := m.res == nil && m.prompts == nil && m.instructions == ""
	m.resMu.RUnlock()
	if !clearedRes {
		t.Fatal("modified reload must clear resources/prompts/instructions")
	}
	if h := d.HealthOf("s"); h != nil {
		t.Fatalf("modified reload must clear health, got %+v", h)
	}
}

func TestReloadNoOpEmptyModified(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	_, _, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}})
	if len(modified) != 0 {
		t.Fatalf("identical reload must not modify: %v", modified)
	}
}

func TestServerChangedIgnoresNameAndFromConnector(t *testing.T) {
	base := config.ServerEntry{Command: []string{"true"}, Name: "a", FromConnector: "x"}
	if serverChanged(base, config.ServerEntry{Command: []string{"true"}, Name: "b", FromConnector: "y"}) {
		t.Fatal("Name/FromConnector must be ignored")
	}
	if !serverChanged(base, config.ServerEntry{Command: []string{"false"}}) {
		t.Fatal("command change must count")
	}
	if !serverChanged(base, config.ServerEntry{Command: []string{"true"}, Description: "d"}) {
		t.Fatal("description change must count")
	}
}

func TestServerChangedNilVsEmptyDocumented(t *testing.T) {
	// CURRENT behavior: reflect.DeepEqual treats nil vs empty as changed.
	a := config.ServerEntry{Command: []string{"true"}}
	b := config.ServerEntry{Command: []string{"true"}, Headers: map[string]string{}}
	if !serverChanged(a, b) {
		t.Fatal("CURRENT: nil vs empty Headers counts as changed (DeepEqual)")
	}
	c := config.ServerEntry{Command: []string{"true"}}
	dd := config.ServerEntry{Command: []string{"true"}}
	if serverChanged(c, dd) {
		t.Fatal("identical entries must not count as changed")
	}
}

// Reload under concurrent read load must not race or drop readers
// (exercises Entry copy + nil-entry skips).
func TestEntryVsReloadRace(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = d.Entry("s")
				_ = d.Names()
				_ = d.HealthOf("s")
			}
		}()
	}
	for j := 0; j < 20; j++ {
		d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
			"s": {Command: []string{"true"}},
		}})
		d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
			"s": {Command: []string{"false"}},
		}})
	}
	wg.Wait()
}

func TestResetClearsSpawnErr(t *testing.T) {
	// Regression: a failed spawn must not wedge later connects after a
	// reset (e.g. login → resetClient → ListTools serves the stale error).
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"/nonexistent-oxyp-spawnerr-test"}},
	}}, testLog())
	defer d.Close()
	m := d.servers["s"]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.clientFor(ctx, m); err == nil {
		t.Fatal("bad command must fail")
	}
	m.mu.Lock()
	sticky := m.spawnErr != nil
	m.mu.Unlock()
	if !sticky {
		t.Fatal("spawn failure must stick for the cohort")
	}
	d.resetClient(m)
	m.mu.Lock()
	cleared := m.spawnErr == nil
	m.mu.Unlock()
	if !cleared {
		t.Fatal("resetClient must clear spawnErr")
	}
}
