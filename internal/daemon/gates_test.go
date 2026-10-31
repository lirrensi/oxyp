package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/metacache"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestCheckParamsGate(t *testing.T) {
	cfg := config.ServerEntry{Name: "s", Params: map[string][]string{
		"create_issue": {"title", "body"},
	}}
	if err := checkParams(cfg, "create_issue", map[string]any{"title": "x", "body": "y"}); err != nil {
		t.Fatalf("allowed params rejected: %v", err)
	}
	if err := checkParams(cfg, "create_issue", map[string]any{"title": "x", "evil": 1}); err == nil {
		t.Fatal("extra param must be denied")
	} else if _, ok := err.(*ParamDeniedError); !ok {
		t.Fatalf("want *ParamDeniedError, got %T", err)
	}
	if err := checkParams(cfg, "other_tool", map[string]any{"anything": 1}); err != nil {
		t.Fatalf("unlisted tool must pass: %v", err)
	}
	if err := checkParams(cfg, "create_issue", nil); err != nil {
		t.Fatalf("empty args must pass: %v", err)
	}
}

// effectiveTimeout: per-server override wins, 0/nil fall back to default.
func TestEffectiveTimeoutGate(t *testing.T) {
	d := 45
	cfg := config.ServerEntry{Name: "s", ToolTimeout: &d}
	if got := effectiveTimeout(cfg, DefaultRequestTimeout); got != 45*1000000000 {
		t.Fatalf("want 45s, got %v", got)
	}
	zero := 0
	if got := effectiveTimeout(config.ServerEntry{ToolTimeout: &zero}, DefaultRequestTimeout); got != DefaultRequestTimeout {
		t.Fatalf("0 must fall back to default, got %v", got)
	}
	if got := effectiveTimeout(config.ServerEntry{}, DefaultRequestTimeout); got != DefaultRequestTimeout {
		t.Fatalf("nil must fall back to default, got %v", got)
	}
}

// remappedHeaders: static Authorization moves under TokenHeader.
func TestRemappedHeadersGate(t *testing.T) {
	cfg := config.ServerEntry{Name: "s", Headers: map[string]string{
		"Authorization": "Bearer x", "X-Other": "y",
	}, TokenHeader: "X-API-Key"}
	out := remappedHeaders(cfg)
	if _, ok := out["Authorization"]; ok {
		t.Fatal("Authorization must be remapped away")
	}
	if out["X-API-Key"] != "Bearer x" || out["X-Other"] != "y" {
		t.Fatalf("bad remap: %v", out)
	}
	if got := remappedHeaders(config.ServerEntry{Headers: map[string]string{"A": "b"}}); got["A"] != "b" {
		t.Fatal("no TokenHeader must pass through")
	}
	// case-insensitive source match
	cfg2 := config.ServerEntry{Name: "s", Headers: map[string]string{"authorization": "Bearer z"}, TokenHeader: "X-K"}
	if out := remappedHeaders(cfg2); out["X-K"] != "Bearer z" {
		t.Fatalf("lowercase authorization must remap: %v", out)
	}
}

// GroupMembers/InGroup: unknown group fails closed, missing members skipped.
func TestGroupMembersGate(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerEntry{
			"a": {Command: []string{"true"}},
			"b": {Command: []string{"true"}},
		},
		Groups: map[string]config.GroupEntry{
			"g": {Servers: []string{"a", "ghost"}},
		},
	}
	d := New(cfg, nil)
	defer d.Close()
	members, ok := d.GroupMembers("g")
	if !ok || len(members) != 1 || members[0] != "a" {
		t.Fatalf("want [a], got %v ok=%v", members, ok)
	}
	if _, ok := d.GroupMembers("nope"); ok {
		t.Fatal("unknown group must return false")
	}
	if !d.InGroup("", "a") {
		t.Fatal("empty group must allow all")
	}
	if !d.InGroup("g", "a") {
		t.Fatal("member must be in group")
	}
	if d.InGroup("g", "b") {
		t.Fatal("non-member must be excluded")
	}
	if d.InGroup("nope", "a") {
		t.Fatal("unknown group must fail closed")
	}
}

// intersectTools: newcomers dropped, matching case-insensitive.
func TestIntersectToolsGate(t *testing.T) {
	mk := func(names ...string) []mcp.Tool {
		var out []mcp.Tool
		for _, n := range names {
			out = append(out, mcp.Tool{Name: n})
		}
		return out
	}
	got := intersectTools(mk("A", "b"), mk("a", "B", "C"))
	if len(got) != 2 {
		t.Fatalf("want 2 survivors, got %v", got)
	}
}

// WithPassthrough/passthroughFunc: allowlist + config-wins + no-Auth.
func TestPassthroughPrecedence(t *testing.T) {
	cfg := config.ServerEntry{Name: "s", PassthroughHeaders: []string{"X-Team-Token"},
		Headers: map[string]string{"X-Fixed": "static"}}
	incoming := http.Header{"X-Team-Token": {"t"}, "X-Fixed": {"evil"}, "Authorization": {"Bearer x"}}
	ctx := WithPassthrough(context.Background(), incoming, cfg.PassthroughHeaders)
	fn := passthroughFunc(cfg)
	got := fn(ctx)
	if got["X-Team-Token"] != "t" {
		t.Fatalf("allowlisted header must forward: %v", got)
	}
	if _, ok := got["X-Fixed"]; ok {
		t.Fatalf("config header must win: %v", got)
	}
	if _, ok := got["Authorization"]; ok {
		t.Fatalf("Authorization must never forward: %v", got)
	}
	// empty ctx → nil
	if out := passthroughFunc(cfg)(context.Background()); out != nil {
		t.Fatalf("empty ctx must yield nil: %v", out)
	}
	// no allowlist → nil func
	if fn := passthroughFunc(config.ServerEntry{}); fn != nil {
		t.Fatal("no allowlist must yield nil func")
	}
}

// RefreshTools replaces the baseline even under pin/hide, no intersect.
func TestRefreshToolsReplacesUnderPin(t *testing.T) {
	msrv := mcpserver.NewMCPServer("r", "1.0")
	msrv.AddTool(mcp.Tool{Name: "v2", Description: "new"}, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}}, nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(msrv))
	defer ts.Close()

	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"r": {URL: ts.URL + "/mcp", Pinned: true, NewTools: "hide"},
	}}
	d := NewWithDirs(cfg, testLog(), "", filepath.Join(t.TempDir(), "cache.json"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed a stale baseline.
	d.cache.Put("r", metacache.Fingerprint(d.servers["r"].getCfg()), []mcp.Tool{{Name: "v1"}})

	tools, added, removed, err := d.RefreshTools(ctx, "r")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "v2" {
		t.Fatalf("want live v2, got %+v", tools)
	}
	if len(added) != 1 || added[0] != "v2" || len(removed) != 1 || removed[0] != "v1" {
		t.Fatalf("bad diff: added=%v removed=%v", added, removed)
	}
	got, ok := d.cache.Get("r", metacache.Fingerprint(d.servers["r"].getCfg()))
	if !ok || len(got) != 1 || got[0].Name != "v2" {
		t.Fatalf("disk must hold refreshed baseline: %v %v", got, ok)
	}
}

// RefreshTools failure keeps the old baseline.
func TestRefreshToolsFailureKeepsBaseline(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"r": {URL: "http://127.0.0.1:1/mcp"},
	}}, testLog())
	defer d.Close()
	m := d.servers["r"]
	m.toolsMu.Lock()
	m.tools = []mcp.Tool{{Name: "old"}}
	m.toolsCachedAt = time.Now()
	m.toolsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, _, _, err := d.RefreshTools(ctx, "r"); err == nil {
		t.Fatal("want fetch error")
	}
	m.toolsMu.RLock()
	defer m.toolsMu.RUnlock()
	if len(m.tools) != 1 || m.tools[0].Name != "old" {
		t.Fatalf("baseline must survive failed refresh: %+v", m.tools)
	}
}

// Push invalidation keeps pinned disk, drops unpinned disk.
func TestPinSurvivesPushInvalidation(t *testing.T) {
	dir := t.TempDir()
	cache := metacache.New(filepath.Join(dir, "cache.json"))
	pinCfg := config.ServerEntry{Name: "p", Command: []string{"true"}, Pinned: true}
	freeCfg := config.ServerEntry{Name: "f", Command: []string{"true"}}
	d := NewWithDirs(&config.Config{Servers: map[string]config.ServerEntry{"p": pinCfg, "f": freeCfg}}, testLog(), "", filepath.Join(dir, "cache.json"))
	defer d.Close()
	d.cache.Put("p", metacache.Fingerprint(pinCfg), []mcp.Tool{{Name: "pinned-tool"}})
	d.cache.Put("f", metacache.Fingerprint(freeCfg), []mcp.Tool{{Name: "free-tool"}})
	d.servers["p"].tools = []mcp.Tool{{Name: "pinned-tool"}}
	d.servers["f"].tools = []mcp.Tool{{Name: "free-tool"}}

	d.InvalidateTools("p")
	d.InvalidateTools("f")

	if _, ok := cache.Get("p", metacache.Fingerprint(pinCfg)); !ok {
		// cache variable was opened before daemon wrote; re-open
		c2 := metacache.New(filepath.Join(dir, "cache.json"))
		if _, ok := c2.Get("p", metacache.Fingerprint(pinCfg)); !ok {
			t.Fatal("pinned disk snapshot must survive push")
		}
		if _, ok := c2.Get("f", metacache.Fingerprint(freeCfg)); ok {
			t.Fatal("unpinned disk must drop on push")
		}
	}
}

// Unknown server refresh errors.
func TestRefreshToolsUnknown(t *testing.T) {
	d := New(&config.Config{}, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, err := d.RefreshTools(ctx, "nope"); err == nil {
		t.Fatal("want not-found error")
	}
}
