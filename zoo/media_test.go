package zoo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/lirrensi/oxyp/internal/meta"
)

// Media must survive daemon → HTTP → shim as native blocks (research:
// textifying base64 costs ~15-25k tokens vs ~1.6k native; some clients
// only render native image blocks).
func TestMediaPassthrough(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"zoo": {Command: []string{"python3", zooPath(t, "stdio", "zoo_stdio.py"), "--media"}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := d.CallTool(ctx, "zoo", "echo_tool", nil)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if len(res.Content) != 3 {
		t.Fatalf("want 3 content blocks, got %d: %+v", len(res.Content), res.Content)
	}
	// block types via JSON round-trip (mcp-go Content is an interface)
	raw, _ := json.Marshal(res.Content)
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatal(err)
	}
	if blocks[0]["type"] != "text" || blocks[1]["type"] != "image" || blocks[2]["type"] != "resource_link" {
		t.Fatalf("block types mangled: %v", raw)
	}
	if blocks[1]["data"] != "iVBORw0KGgoAAAANSUhEUgAAAAEA" || blocks[1]["mimeType"] != "image/png" {
		t.Fatalf("image block mangled: %v", blocks[1])
	}
	if res.StructuredContent == nil {
		t.Fatal("structuredContent dropped by daemon")
	}

	// meta layer must preserve blocks + structuredContent
	srv := meta.New(d, func() (a, r, m []string, err error) { return nil, nil, nil, nil }, "", "127.0.0.1:8379")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/servers/zoo/tools/echo_tool", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	content, ok := body["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("meta content mangled: %v", body)
	}
	if content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("meta image block mangled: %v", content[1])
	}
	if sc, ok := body["structuredContent"].(map[string]any); !ok || sc["format"] != "png" {
		t.Fatalf("meta structuredContent dropped: %v", body)
	}
}
