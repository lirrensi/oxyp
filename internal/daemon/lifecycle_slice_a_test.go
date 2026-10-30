package daemon

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"oxyp/internal/config"
)

// Aged remote memory cache revalidates live exactly once.
func TestRemoteTTLRevalidatesLive(t *testing.T) {
	msrv := mcpserver.NewMCPServer("ttl", "1.0")
	msrv.AddTool(mcp.Tool{Name: "live", Description: "from server"}, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}}, nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(msrv))
	defer ts.Close()

	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"r": {URL: ts.URL + "/mcp"},
	}}, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed memory with a stale entry (old tools, aged timestamp).
	m := d.servers["r"]
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "stale"}}
	m.toolsCachedAt = time.Now().Add(-10 * time.Minute)
	m.toolsMu.Unlock()

	tools, err := d.ListTools(ctx, "r")
	if err != nil {
		t.Fatalf("revalidation: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "live" {
		t.Fatalf("want live tools, got %+v", tools)
	}
}

// Aged remote cache + dead upstream serves stale instead of failing.
func TestRemoteTTLServesStaleOnFailure(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"r": {URL: "http://127.0.0.1:1/mcp"},
	}}, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m := d.servers["r"]
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "stale"}}
	m.toolsCachedAt = time.Now().Add(-10 * time.Minute)
	m.toolsMu.Unlock()

	tools, err := d.ListTools(ctx, "r")
	if err != nil {
		t.Fatalf("stale serve must not fail: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "stale" {
		t.Fatalf("want stale tools, got %+v", tools)
	}
}

// Local memory hits never revalidate (no TTL for stdio children).
func TestLocalCacheNoTTL(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := d.servers["s"]
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "mem"}}
	m.toolsCachedAt = time.Now().Add(-time.Hour)
	m.toolsMu.Unlock()

	tools, err := d.ListTools(ctx, "s")
	if err != nil || len(tools) != 1 || tools[0].Name != "mem" {
		t.Fatalf("local aged hit must serve memory: %+v %v", tools, err)
	}
	if d.IsConnected("s") {
		t.Fatal("local TTL path must not spawn")
	}
}

// Reload bumps gen for modified entries only.
func TestReloadBumpsGenOnModify(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"a": {Command: []string{"true"}},
		"b": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	ga0 := d.servers["a"].getCfg()
	_ = ga0
	genA0 := func() int { d.servers["a"].mu.Lock(); defer d.servers["a"].mu.Unlock(); return d.servers["a"].gen }()
	genB0 := func() int { d.servers["b"].mu.Lock(); defer d.servers["b"].mu.Unlock(); return d.servers["b"].gen }()

	d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"a": {Command: []string{"false"}},
		"b": {Command: []string{"true"}},
	}})

	genA1 := func() int { d.servers["a"].mu.Lock(); defer d.servers["a"].mu.Unlock(); return d.servers["a"].gen }()
	genB1 := func() int { d.servers["b"].mu.Lock(); defer d.servers["b"].mu.Unlock(); return d.servers["b"].gen }()
	if genA1 != genA0+1 {
		t.Fatalf("modified entry must bump gen: %d -> %d", genA0, genA1)
	}
	if genB1 != genB0 {
		t.Fatalf("unchanged entry must keep gen: %d -> %d", genB0, genB1)
	}
}

// Entry returns a copy: mutating it must not race or affect the live entry.
func TestEntryReturnsCopy(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	e := d.Entry("s")
	e.Command = []string{"evil"}
	if got := d.Entry("s").Command; len(got) != 1 || got[0] != "true" {
		t.Fatalf("live entry mutated via copy: %v", got)
	}
}

// IsConnected/CachedInstructions never spawn.
func TestListAPIsNeverSpawn(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	if d.IsConnected("s") {
		t.Fatal("cold server must not be connected")
	}
	if _, ok := d.CachedInstructions("s"); ok {
		t.Fatal("cold server must have no cached instructions")
	}
	m := d.servers["s"]
	m.mu.Lock()
	spawned := m.client != nil
	m.mu.Unlock()
	if spawned {
		t.Fatal("list APIs must never spawn")
	}
	if d.IsConnected("missing") {
		t.Fatal("unknown server must not be connected")
	}
}

// AuthNeededError carries the login hint when no URL is set.
func TestAuthNeededErrorHint(t *testing.T) {
	e := (&AuthNeededError{Server: "s"}).Error()
	if len(e) == 0 || !contains(e, "oxyp login s") {
		t.Fatalf("want login hint, got %q", e)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
