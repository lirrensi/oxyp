package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"oxyp/internal/config"
	"oxyp/internal/metacache"
)

// sweepIdle must stop a stale local child but keep its discovery cache.
func TestSweepStopsIdleLocal(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"idle": {Command: []string{"true"}, IdleTimeout: new(10)},
	}}
	d := New(cfg, testLog())
	m := d.servers["idle"]
	// fake a connected, long-idle child with cached tools
	m.mu.Lock()
	m.client = &fakeClient{}
	m.lastUsed = time.Now().Add(-11 * time.Minute)
	m.mu.Unlock()
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "t"}}
	m.toolsMu.Unlock()

	d.sweepIdle()

	m.mu.Lock()
	stillConnected := m.client != nil
	m.mu.Unlock()
	if stillConnected {
		t.Fatal("idle child not stopped")
	}
	m.toolsMu.RLock()
	kept := len(m.tools)
	m.toolsMu.RUnlock()
	if kept != 1 {
		t.Fatal("sweep must keep discovery cache")
	}
}

func TestSweepSkipsInflightAndFresh(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"busy":  {Command: []string{"true"}, IdleTimeout: new(10)},
		"fresh": {Command: []string{"true"}, IdleTimeout: new(10)},
		"keep":  {Command: []string{"true"}, Lifecycle: "keep-alive"},
		"eager": {Command: []string{"true"}, Lifecycle: "eager"},
		"never": {Command: []string{"true"}, IdleTimeout: new(0)},
		"rem":   {URL: "https://x/mcp", IdleTimeout: new(10)},
	}}
	d := New(cfg, testLog())
	old := time.Now().Add(-time.Hour)
	for _, m := range d.servers {
		m.mu.Lock()
		m.client = &fakeClient{}
		m.lastUsed = old
		m.mu.Unlock()
	}
	d.servers["busy"].mu.Lock()
	d.servers["busy"].inFlight = 1
	d.servers["busy"].mu.Unlock()
	d.servers["fresh"].mu.Lock()
	d.servers["fresh"].lastUsed = time.Now()
	d.servers["fresh"].mu.Unlock()

	d.sweepIdle()

	for name, m := range d.servers {
		m.mu.Lock()
		connected := m.client != nil
		m.mu.Unlock()
		if !connected {
			t.Errorf("server %q wrongly swept", name)
		}
	}
}

func TestDiskColdList(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	entry := config.ServerEntry{Command: []string{"true"}}
	fp := metacache.Fingerprint(entry)
	c := metacache.New(cachePath)
	c.Put("s", fp, []mcp.Tool{{Name: "cached", Description: "from disk"}})

	cfg := &config.Config{Servers: map[string]config.ServerEntry{"s": entry}}
	d := NewWithDirs(cfg, testLog(), "", cachePath)
	tools, err := d.ListTools(context.Background(), "s")
	if err != nil {
		t.Fatalf("disk-cold list failed: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "cached" {
		t.Fatalf("wrong tools: %+v", tools)
	}
	// must not have spawned (true exits immediately; spawn would fail init)
	d.servers["s"].mu.Lock()
	spawned := d.servers["s"].client != nil
	d.servers["s"].mu.Unlock()
	if spawned {
		t.Fatal("disk hit must not spawn")
	}
}

func TestEagerStartConnects(t *testing.T) {
	// eager against a bad command records failure, never fatal
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"e": {Command: []string{"/nonexistent-oxyp-test-binary"}, Lifecycle: "eager"},
		"l": {Command: []string{"/nonexistent-oxyp-test-binary"}},
	}}
	d := New(cfg, testLog())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d.EagerStart(ctx) // must return, not hang or panic
	if h := d.HealthOf("e"); h == nil || h.Error == "" {
		t.Fatalf("eager failure must record health: %+v", h)
	}
	if h := d.HealthOf("l"); h != nil {
		t.Fatalf("lazy server must stay untouched: %+v", h)
	}
}

// fakeClient is a connected-but-dumb MCPClient for sweep tests.
type fakeClient struct {
	client.MCPClient // nil embed: only Close is ever called
}

func (f *fakeClient) Close() error { return nil }
