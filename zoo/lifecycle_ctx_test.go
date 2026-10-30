// Slice A regression tests: daemon-lifetime spawn context, reload
// generation, SSE idle beyond reqTO, and remote TTL revalidation.
package zoo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"oxyp/internal/config"
	"oxyp/internal/daemon"
	"oxyp/internal/meta"
)

// Two successive daemon calls with an INTERMEDIATE-CANCELLED ctx both
// succeed: the first RPC's cancellation must not kill the child.
func TestLifecycleCtxSurvivesCancelledRPC(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"zoo": {Command: []string{"python3", zooPath(t, "stdio", "zoo_stdio.py")}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()

	// First call on a ctx cancelled right after it returns.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := d.ListTools(ctx1, "zoo"); err != nil {
		cancel1()
		t.Fatalf("first list: %v", err)
	}
	cancel1() // simulate ServeHTTP returning: request ctx dies here

	// Second call on a fresh ctx must still succeed (child survived).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	res, err := d.CallTool(ctx2, "zoo", "echo_tool", map[string]any{"msg": "again"})
	if err != nil {
		t.Fatalf("second call after ctx cancel: %v", err)
	}
	if res.IsError {
		t.Fatalf("second call isError: %+v", res.Content)
	}
	if !d.IsConnected("zoo") {
		t.Fatal("server should still be connected")
	}
}

// Same guarantee through the HTTP layer: two successive POSTs, first
// response fully consumed (ctx cancelled), second still succeeds.
func TestLifecycleCtxOverHTTP(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"zoo": {Command: []string{"python3", zooPath(t, "stdio", "zoo_stdio.py")}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	srv := meta.New(d, func() (a, r, m []string, err error) { return nil, nil, nil, nil }, "", "127.0.0.1:8379")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(tool string) int {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/servers/zoo/tools/"+tool, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", tool, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if st := post("echo_tool"); st != http.StatusOK {
		t.Fatalf("first POST = %d", st)
	}
	// First request ctx is now cancelled (response consumed).
	if st := post("ping_tool"); st != http.StatusOK {
		t.Fatalf("second POST after first ctx death = %d, want 200", st)
	}
}

// SSE idle longer than reqTO, then a call succeeds: streaming uses a
// Timeout:0 client, per-RPC ctx bounds the call.
func TestSSEIdleBeyondReqTO(t *testing.T) {
	port := freePort(t)
	startZooHTTP(t, "legacy-sse", port)
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"h": {URL: fmt.Sprintf("http://127.0.0.1:%d/sse", port), Transport: "sse"},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := d.ListTools(ctx, "h"); err != nil {
		t.Fatalf("list: %v", err)
	}
	// Idle longer than the 10s RPC timeout; the SSE stream must survive.
	select {
	case <-time.After(12 * time.Second):
	case <-ctx.Done():
		t.Fatal("ctx expired during idle")
	}
	res, err := d.CallTool(ctx, "h", "http_echo", map[string]any{"msg": "after-idle"})
	if err != nil || res.IsError {
		t.Fatalf("call after 12s idle: %v %+v", err, res)
	}
}

// Reload during use adopts the new config: bump gen for modified entries;
// an in-flight spawn from the old config is discarded.
func TestReloadAdoptsNewConfig(t *testing.T) {
	dir := t.TempDir()
	py := "python3"
	mkEntry := func(args ...string) config.ServerEntry {
		return config.ServerEntry{Command: append([]string{py, filepath.Join(zooPath(t), "stdio", "zoo_stdio.py")}, args...)}
	}
	_ = dir
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"s": mkEntry(),
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := d.ListTools(ctx, "s"); err != nil {
		t.Fatalf("list: %v", err)
	}
	before := d.IsConnected("s")

	// Reload with a modified entry (different argv): must report modified
	// and drop the old connection.
	_, _, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": mkEntry("--blank-stdout"),
	}})
	if len(modified) != 1 || modified[0] != "s" {
		t.Fatalf("modified = %v", modified)
	}
	if before && d.IsConnected("s") {
		t.Fatal("modified reload must drop the old connection")
	}
	// New config is in effect: list works against the new child.
	tools, err := d.ListTools(ctx, "s")
	if err != nil || len(tools) == 0 {
		t.Fatalf("list after reload: tools=%d err=%v", len(tools), err)
	}
}

// (no revalidation before RemoteToolsTTL). Aged-revalidation is covered
// by in-package daemon tests (they can set toolsCachedAt directly).
func TestRemoteTTLServesFreshWhenDead(t *testing.T) {
	msrv := mcpserver.NewMCPServer("ttl", "1.0")
	msrv.AddTool(mcp.Tool{Name: "t1", Description: "v1"}, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "v1"}}}, nil
	})
	hsrv := mcpserver.NewStreamableHTTPServer(msrv)
	ts := httptest.NewServer(hsrv)

	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"r": {URL: ts.URL + "/mcp"},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tools, err := d.ListTools(ctx, "r")
	if err != nil || len(tools) != 1 {
		t.Fatalf("initial list: tools=%d err=%v", len(tools), err)
	}
	ts.Close() // upstream dead; memory is fresh → serve without dialing
	tools, err = d.ListTools(ctx, "r")
	if err != nil || len(tools) != 1 || tools[0].Name != "t1" {
		t.Fatalf("fresh hit with dead upstream must serve memory: %+v %v", tools, err)
	}
}
