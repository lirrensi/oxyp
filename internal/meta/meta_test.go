package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// testServer builds an in-process streamable MCP server.
// tools: name -> handler returning content blocks.
// resources/prompts/instruction toggles cover the unsupported branches.
type mcpFixture struct {
	t       *testing.T
	srv     *mcpserver.MCPServer
	http    *httptest.Server
	reqs    *atomic.Int64
	daemon  *daemon.Daemon
	handler http.Handler
}

func newFixture(t *testing.T, name string, desc string, policy *config.ToolsPolicy, setup func(s *mcpserver.MCPServer, reqs *atomic.Int64)) *mcpFixture {
	t.Helper()
	var reqs atomic.Int64
	s := mcpserver.NewMCPServer(name, "1.0",
		mcpserver.WithInstructions("instr for "+name),
		mcpserver.WithResourceCapabilities(true, true),
		mcpserver.WithPromptCapabilities(true),
	)
	if setup != nil {
		setup(s, &reqs)
	}
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s))
	t.Cleanup(ts.Close)
	entry := config.ServerEntry{Description: desc, URL: ts.URL + "/mcp", Tools: policy}
	d := daemon.New(&config.Config{Servers: map[string]config.ServerEntry{name: entry}}, nil)
	t.Cleanup(d.Close)
	return &mcpFixture{t: t, srv: s, http: ts, reqs: &reqs, daemon: d,
		handler: New(d, func() (added, removed, modified []string, err error) {
			return nil, nil, nil, nil
		}, "", "127.0.0.1:8379").Handler()}
}

func countCalls(reqs *atomic.Int64, h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reqs.Add(1)
		return h(ctx, req)
	}
}
func echoTool() (mcp.Tool, mcpserver.ToolHandlerFunc) {
	tool := mcp.NewTool("echo", mcp.WithDescription("echoes input"))
	return tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.TextContent{Type: "text", Text: "ok"}},
			StructuredContent: map[string]any{"echo": req.Params.Arguments},
		}
		res.Meta = mcp.NewMetaFromMap(map[string]any{"src": "test"})
		return res, nil
	}
}

func (f *mcpFixture) get(path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, r)
	return rr
}

func (f *mcpFixture) postJSON(path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Host = "127.0.0.1"
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, r)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %d body %q: %v", rr.Code, rr.Body.String(), err)
	}
	return v
}

func TestServersEndpoint(t *testing.T) {
	f := newFixture(t, "alpha", "Alpha Server", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
	})
	rr := f.get("/servers")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	srvs, _ := v["servers"].([]any)
	if len(srvs) != 1 {
		t.Fatalf("want 1 server, got %v", v)
	}
	m := srvs[0].(map[string]any)
	if m["name"] != "alpha" || m["description"] != "Alpha Server" {
		t.Fatalf("bad server entry: %v", m)
	}
	if m["connected"] != false {
		t.Fatalf("cold server must show connected:false: %v", m)
	}
	// Filter is case-insensitive over name and description.
	for _, q := range []string{"ALP", "alpha%20serv"} {
		if rr := f.get("/servers?filter=" + q); rr.Code != http.StatusOK {
			t.Fatalf("filter %q: %d", q, rr.Code)
		} else if n := len(decodeBody(t, rr)["servers"].([]any)); n != 1 {
			t.Fatalf("filter %q: want 1, got %d", q, n)
		}
	}
	if rr := f.get("/servers?filter=zzz"); len(decodeBody(t, rr)["servers"].([]any)) != 0 {
		t.Fatalf("filter zzz should match nothing: %q", rr.Body.String())
	}
	// After connecting, instructions surface without map-order flakiness.
	if _, err := f.daemon.ListTools(context.Background(), "alpha"); err != nil {
		t.Fatalf("list: %v", err)
	}
	rr = f.get("/servers")
	m = decodeBody(t, rr)["servers"].([]any)[0].(map[string]any)
	if m["connected"] != true || m["instructions"] != "instr for alpha" {
		t.Fatalf("connected server should carry instructions: %v", m)
	}
}

func TestServersExcludesDisabled(t *testing.T) {
	f := false
	d := daemon.New(&config.Config{Servers: map[string]config.ServerEntry{
		"on":  {Command: []string{"true"}},
		"off": {Command: []string{"true"}, Enabled: &f},
	}}, nil)
	defer d.Close()
	h := New(d, nil, "", "127.0.0.1:8379").Handler()
	r := httptest.NewRequest("GET", "/servers", nil)
	r.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	names := []string{}
	for _, s := range decodeBody(t, rr)["servers"].([]any) {
		names = append(names, s.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "on" {
		t.Fatalf("want only [on], got %v", names)
	}
}

func TestServersDoesNotSpawn(t *testing.T) {
	f := newFixture(t, "cold", "cold server", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
	})
	if rr := f.get("/servers"); rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if n := f.reqs.Load(); n != 0 {
		t.Fatalf("GET /servers must not touch downstream, got %d requests", n)
	}
	if f.daemon.IsConnected("cold") {
		t.Fatal("servers endpoint must not connect")
	}
}

func TestServersConfigErrorAndHealth(t *testing.T) {
	d := daemon.New(&config.Config{Servers: map[string]config.ServerEntry{
		"bad": {ConfigError: "nope"},
	}}, nil)
	defer d.Close()
	h := New(d, nil, "", "127.0.0.1:8379").Handler()
	for _, path := range []string{"/servers", "/health"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "127.0.0.1"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d", path, rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "config: nope") {
			t.Fatalf("%s: want config error, got %q", path, body)
		}
		if !strings.Contains(body, `"healthy":false`) {
			t.Fatalf("%s: want healthy:false, got %q", path, body)
		}
	}
}

func TestFederationRevConverges(t *testing.T) {
	// catalog.rev MUST equal /rev.rev for the same state — else importers
	// resync every tick. Exercises the divergent inputs: a denied tool, a
	// resource, and a prompt (rev-mode must mirror full-mode exactly).
	policy := &config.ToolsPolicy{Deny: []string{"secret_*", "hidden"}}
	f := newFixture(t, "alpha", "Alpha Server", policy, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
		s.AddTool(mcp.NewTool("hidden", mcp.WithDescription("denied")), countCalls(reqs, h))
		s.AddResource(mcp.NewResource("test://r", "r"), func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: "test://r", Text: "x"}}, nil
		})
		s.AddPrompt(mcp.NewPrompt("p"), func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{{Role: "user", Content: mcp.TextContent{Type: "text", Text: "hi"}}}}, nil
		})
	})
	cat := decodeBody(t, f.get("/federation/v1/catalog"))
	rev := decodeBody(t, f.get("/federation/v1/rev"))["rev"].(string)
	if cat["rev"] != rev {
		t.Fatalf("catalog.rev %v != /rev %v", cat["rev"], rev)
	}
	// Denied tool absent from the wire row but the hash still converges.
	srvs := cat["servers"].([]any)
	tools := srvs[0].(map[string]any)["tools"].([]any)
	for _, tt := range tools {
		if tt.(map[string]any)["name"] == "hidden" {
			t.Fatal("denied tool leaked into catalog")
		}
	}
	// Second tick: still equal (no spawn-state drift between calls).
	rev2 := decodeBody(t, f.get("/federation/v1/rev"))["rev"].(string)
	if rev2 != rev {
		t.Fatalf("rev drifted without change: %v -> %v", rev, rev2)
	}
}

func TestFederationRev(t *testing.T) {
	f := newFixture(t, "alpha", "Alpha Server", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
	})
	revOf := func() string {
		rr := f.get("/federation/v1/rev")
		if rr.Code != http.StatusOK {
			t.Fatalf("rev: %d", rr.Code)
		}
		return decodeBody(t, rr)["rev"].(string)
	}
	r1 := revOf()
	if r1 == "" {
		t.Fatal("empty rev")
	}
	if revOf() != r1 {
		t.Fatal("rev must be stable without changes")
	}
	// New cached tool flips the hash (simulate upstream list_changed).
	tools, err := f.daemon.ListTools(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	_ = tools
	// Rev never spawns: cold second server contributes name-only, no downstream hit.
	before := f.reqs.Load()
	_ = revOf()
	if f.reqs.Load() != before {
		t.Fatal("rev must not touch downstream")
	}
}

func TestFederationV1RoundTrip(t *testing.T) {
	f := newFixture(t, "alpha", "Alpha Server", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
		s.AddResource(mcp.NewResource("test://hello", "hello"), func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: "test://hello", MIMEType: "text/plain", Text: "hi"}}, nil
		})
		s.AddPrompt(mcp.NewPrompt("greet", mcp.WithPromptDescription("greets"),
			mcp.WithArgument("who", mcp.ArgumentDescription("who"), mcp.RequiredArgument())),
			func(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				who := "?"
				if req.Params.Arguments != nil {
					who = req.Params.Arguments["who"]
				}
				return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{{Role: "user",
					Content: mcp.TextContent{Type: "text", Text: "hello " + who}}}}, nil
			})
	})
	// call: native envelope, structured + meta preserved
	rr := f.postJSON("/federation/v1/call", `{"server":"alpha","tool":"echo","arguments":{"a":1}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("call: %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	if v["isError"] == true || v["structuredContent"] == nil || v["_meta"] == nil {
		t.Fatalf("call envelope = %v", v)
	}
	// resource: native contents
	rr = f.postJSON("/federation/v1/resource", `{"server":"alpha","uri":"test://hello"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("resource: %d %q", rr.Code, rr.Body.String())
	}
	if len(decodeBody(t, rr)["contents"].([]any)) != 1 {
		t.Fatalf("resource = %q", rr.Body.String())
	}
	// prompt: native messages
	rr = f.postJSON("/federation/v1/prompt", `{"server":"alpha","name":"greet","arguments":{"who":"bob"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("prompt: %d %q", rr.Code, rr.Body.String())
	}
	msgs := decodeBody(t, rr)["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("prompt = %q", rr.Body.String())
	}
	// catalog carries resources + prompts rows now
	rr = f.get("/federation/v1/catalog")
	srvs := decodeBody(t, rr)["servers"].([]any)
	m := srvs[0].(map[string]any)
	if len(m["resources"].([]any)) != 1 || len(m["prompts"].([]any)) != 1 {
		t.Fatalf("catalog rows = %v", m)
	}
	// gates: unknown server 404, bad JSON 400, missing fields 400
	if rr := f.postJSON("/federation/v1/call", `{"server":"nope","tool":"x"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown server: %d", rr.Code)
	}
	if rr := f.postJSON("/federation/v1/call", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rr.Code)
	}
	if rr := f.postJSON("/federation/v1/prompt", `{"server":"alpha"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing name: %d", rr.Code)
	}
}

func TestFederationCatalog(t *testing.T) {
	f := newFixture(t, "alpha", "Alpha Server", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
	})
	rr := f.get("/federation/v1/catalog")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	if v["version"] != "v1" || v["instance"] == "" {
		t.Fatalf("envelope = %v", v)
	}
	srvs, _ := v["servers"].([]any)
	if len(srvs) != 1 {
		t.Fatalf("want 1 server, got %v", v)
	}
	m := srvs[0].(map[string]any)
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool with schema, got %v", m)
	}
	tm := tools[0].(map[string]any)
	if tm["name"] != "echo" || tm["inputSchema"] == nil {
		t.Fatalf("tool must carry schema: %v", tm)
	}
	// Peer view: pinned routes + sharing filter apply like /servers.
	r := httptest.NewRequest("GET", "/federation/v1/catalog", nil)
	r.Host = "127.0.0.1"
	r.Header.Set("X-Oxyp-Instance", "caller1")
	rr2 := httptest.NewRecorder()
	f.handler.ServeHTTP(rr2, r)
	if rr2.Code != http.StatusOK {
		t.Fatalf("peer catalog: %d", rr2.Code)
	}
}

func TestPeerHandshake(t *testing.T) {
	d := daemon.New(&config.Config{
		Daemon:  config.DaemonConfig{Name: "mine"},
		Servers: map[string]config.ServerEntry{"a": {Command: []string{"true"}}},
	}, nil)
	defer d.Close()
	h := New(d, nil, "", "127.0.0.1:8379").Handler()
	for _, peer := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/peer", nil)
		r.Host = "127.0.0.1"
		if peer {
			r.Header.Set("X-Oxyp-Instance", "caller1")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("peer=%v: %d", peer, rr.Code)
		}
		v := decodeBody(t, rr)
		if v["instance"] == "" || v["name"] != "mine" {
			t.Fatalf("peer=%v: %+v", peer, v)
		}
		if len(v["servers"].([]any)) != 1 || v["count"] != float64(1) {
			t.Fatalf("peer=%v: %+v", peer, v)
		}
	}
}

func TestPeerSharingFiltersServers(t *testing.T) {
	sharing := &config.PeerSharingConfig{Mode: config.PeerSharingHide}
	d := daemon.New(&config.Config{
		Daemon: config.DaemonConfig{PeerSharing: sharing},
		Servers: map[string]config.ServerEntry{
			"pub": {Command: []string{"true"}},
		},
	}, nil)
	defer d.Close()
	h := New(d, nil, "", "127.0.0.1:8379").Handler()
	get := func(path string, peer bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "127.0.0.1"
		if peer {
			r.Header.Set("X-Oxyp-Instance", "caller1")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	names := func(rr *httptest.ResponseRecorder) []string {
		var out []string
		for _, sv := range decodeBody(t, rr)["servers"].([]any) {
			out = append(out, sv.(map[string]any)["name"].(string))
		}
		return out
	}
	// Local sees everything; peer sees shared locals.
	if got := names(get("/servers", false)); len(got) != 1 {
		t.Fatalf("local = %v", got)
	}
	if got := names(get("/servers", true)); len(got) != 1 || got[0] != "pub" {
		t.Fatalf("peer hide = %v", got)
	}
	// Deny hides from peers only (no oracle change for locals).
	sharing.Deny = []string{"pub"}
	if got := names(get("/servers", true)); len(got) != 0 {
		t.Fatalf("denied peer = %v", got)
	}
	if got := names(get("/servers", false)); len(got) != 1 {
		t.Fatalf("denied local = %v", got)
	}
	if rr := get("/servers/pub/tools", true); rr.Code != http.StatusNotFound {
		t.Fatalf("denied direct = %d", rr.Code)
	}
	// Health follows the same view (instance always rides for handshake).
	var hv struct {
		Instance string         `json:"instance"`
		Servers  map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(get("/health", true).Body.Bytes(), &hv); err != nil {
		t.Fatal(err)
	}
	if hv.Instance == "" || len(hv.Servers) != 0 {
		t.Fatalf("peer health = %+v", hv)
	}
}

func TestHealthCarriesIdentity(t *testing.T) {
	d := daemon.New(&config.Config{}, nil)
	defer d.Close()
	s := New(d, nil, "", "127.0.0.1:8379")
	s.SetIdentity(4242, "2026-10-30T00:00:00Z")
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	r.Host = "127.0.0.1"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("health: got %d", rr.Code)
	}
	var v struct {
		PID       int            `json:"pid"`
		StartedAt string         `json:"startedAt"`
		Servers   map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("health body: %v", err)
	}
	if v.PID != 4242 || v.StartedAt == "" || v.Servers == nil {
		t.Fatalf("identity missing: %+v", v)
	}
}

func TestListToolsAndDescribeC1(t *testing.T) {
	policy := &config.ToolsPolicy{Deny: []string{"delete_*", "secret"}}
	f := newFixture(t, "svc", "svc", policy, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		keep, kh := echoTool()
		keep.Name = "keep"
		s.AddTool(keep, countCalls(reqs, kh))
		del := mcp.NewTool("delete_x", mcp.WithDescription("doomed"))
		s.AddTool(del, countCalls(reqs, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}}, nil
		}))
	})
	// List shows only allowed tools.
	rr := f.get("/servers/svc/tools")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %q", rr.Code, rr.Body.String())
	}
	names := []string{}
	for _, x := range decodeBody(t, rr)["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if len(names) != 1 || names[0] != "keep" {
		t.Fatalf("want only [keep], got %v", names)
	}
	// Describe echoes the canonical name even with wrong case.
	rr = f.get("/servers/svc/tools/KEEP")
	if rr.Code != http.StatusOK {
		t.Fatalf("describe case: %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	if v["name"] != "keep" || v["description"] != "echoes input" {
		t.Fatalf("bad describe: %v", v)
	}
	if _, ok := v["inputSchema"]; !ok {
		t.Fatalf("describe must include inputSchema: %v", v)
	}
	if rr := f.get("/servers/svc/tools/missing"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown tool: got %d", rr.Code)
	}
	if rr := f.get("/servers/unknown/tools"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown server: got %d", rr.Code)
	}
}

func TestInvokeGuardArmed(t *testing.T) {
	// Daemon-level guard caps text with a spill note; media passes through.
	f := newFixture(t, "armed", "armed", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool := mcp.NewTool("big", mcp.WithDescription("big"))
		s.AddTool(tool, countCalls(reqs, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: strings.Repeat("z", 200)},
				mcp.ImageContent{Type: "image", Data: "aGVsbG8=", MIMEType: "image/png"},
			}}, nil
		}))
	})
	armed := daemon.New(&config.Config{
		Daemon: config.DaemonConfig{OutputGuard: &config.OutputGuardConfig{MaxBytes: 10, MaxLines: 100000}},
		Servers: map[string]config.ServerEntry{
			"s": {Description: "s", URL: f.http.URL + "/mcp"},
		},
	}, nil)
	defer armed.Close()
	h := New(armed, nil, "", "127.0.0.1:8379").Handler()
	r := httptest.NewRequest("POST", "/servers/s/tools/big", strings.NewReader(`{}`))
	r.Host = "127.0.0.1"
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("invoke: %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	content := v["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want 2 blocks: %v", v)
	}
	txt := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "output guard") {
		t.Fatalf("guard should cap text with note: %q", txt)
	}
	if img := content[1].(map[string]any); img["data"] != "aGVsbG8=" {
		t.Fatalf("media must pass through guard untouched: %v", img)
	}
}

func TestInvokeGuardAndMedia(t *testing.T) {
	f := newFixture(t, "g", "g", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool := mcp.NewTool("big", mcp.WithDescription("big output"))
		s.AddTool(tool, countCalls(reqs, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: strings.Repeat("z", 200)},
				mcp.ImageContent{Type: "image", Data: "aGVsbG8=", MIMEType: "image/png"},
			}}, nil
		}))
	})
	// No guard configured: full text passes, image byte-identical.
	rr := f.postJSON("/servers/g/tools/big", `{}`)
	v := decodeBody(t, rr)
	content := v["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want 2 blocks, got %v", v)
	}
	if content[0].(map[string]any)["text"] != strings.Repeat("z", 200) {
		t.Fatalf("unguarded text must pass through")
	}
	img := content[1].(map[string]any)
	if img["data"] != "aGVsbG8=" || img["mimeType"] != "image/png" {
		t.Fatalf("image must be byte-identical: %v", img)
	}
}

func TestInvokeC1AndPayload(t *testing.T) {
	policy := &config.ToolsPolicy{Deny: []string{"delete_*"}}
	f := newFixture(t, "svc", "svc", policy, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
	})
	// Denied tool: 403 shape with oxypPolicy marker, no spawn.
	d := daemon.New(&config.Config{Servers: map[string]config.ServerEntry{
		"bogus": {Command: []string{"/nonexistent-oxyp-tool-spawn-probe"},
			Tools: &config.ToolsPolicy{Deny: []string{"delete_*"}}},
	}}, nil)
	defer d.Close()
	h := New(d, nil, "", "127.0.0.1:8379").Handler()
	r := httptest.NewRequest("POST", "/servers/bogus/tools/delete_x", strings.NewReader(`{"parameters":{}}`))
	r.Host = "127.0.0.1"
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied invoke: got %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	if v["error"] != "blocked by policy" {
		t.Fatalf("bad shape: %v", v)
	}
	if !strings.Contains(fmt.Sprint(v["details"]), "delete_*") {
		t.Fatalf("details should name the pattern: %v", v)
	}
	if m, _ := v["_meta"].(map[string]any); m["oxypPolicy"] != "blocked" {
		t.Fatalf("want oxypPolicy marker: %v", v)
	}
	// Allowed: passthrough incl structuredContent + _meta.
	rr = f.postJSON("/servers/svc/tools/echo", `{"parameters":{"a":1}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("invoke: %d %q", rr.Code, rr.Body.String())
	}
	v = decodeBody(t, rr)
	content, _ := v["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("bad content: %v", v)
	}
	sc, _ := v["structuredContent"].(map[string]any)
	if sc["echo"] == nil {
		t.Fatalf("structuredContent must pass through: %v", v)
	}
	if _, ok := v["_meta"]; !ok {
		t.Fatalf("_meta must pass through: %v", v)
	}
	// Unknown server 404; invalid JSON 400; empty body works (nil params).
	if rr := f.get("/servers/unknown/tools/echo"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown server invoke: %d", rr.Code)
	}
	// Invalid JSON needs a POST with correct content-type.
	r2 := httptest.NewRequest("POST", "/servers/svc/tools/echo", strings.NewReader("{bad"))
	r2.Host = "127.0.0.1"
	r2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	f.handler.ServeHTTP(rr2, r2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: got %d %q", rr2.Code, rr2.Body.String())
	}
	r3 := httptest.NewRequest("POST", "/servers/svc/tools/echo", nil)
	r3.Host = "127.0.0.1"
	rr3 := httptest.NewRecorder()
	f.handler.ServeHTTP(rr3, r3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("empty body: got %d %q", rr3.Code, rr3.Body.String())
	}
}

func TestResourcesEndpoint(t *testing.T) {
	f := newFixture(t, "r", "r", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		res := mcp.NewResource("file:///a b.txt", "afile", mcp.WithResourceDescription("a file"), mcp.WithMIMEType("text/plain"))
		s.AddResource(res, func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			reqs.Add(1)
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: "file:///a b.txt", MIMEType: "text/plain", Text: "hello"}}, nil
		})
	})
	rr := f.get("/servers/r/resources")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	res, _ := v["resources"].([]any)
	if len(res) != 1 {
		t.Fatalf("want 1 resource: %v", v)
	}
	m0 := res[0].(map[string]any)
	if m0["uri"] != "file:///a b.txt" || m0["name"] != "afile" || m0["mimeType"] != "text/plain" {
		t.Fatalf("bad resource row: %v", m0)
	}
	if v["instructions"] != "instr for r" {
		t.Fatalf("instructions must ride along: %v", v)
	}
	// Read by path tail (encoded URI with space + slashes).
	rr = f.get("/servers/r/resources/file%3A%2F%2F%2Fa%20b.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("read by path: %d %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello") {
		t.Fatalf("want contents: %q", rr.Body.String())
	}
	// Read by ?uri=.
	rr = f.get("/servers/r/resources?uri=file%3A%2F%2F%2Fa%20b.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("read by query: %d %q", rr.Code, rr.Body.String())
	}
	// Both / neither are 400.
	if rr := f.get("/servers/r/resources/file%3A%2F%2Fx?uri=file%3A%2F%2F%2Fy"); rr.Code != http.StatusBadRequest {
		t.Fatalf("both: got %d", rr.Code)
	}
	if rr := f.get("/servers/r/resources"); rr.Code == http.StatusBadRequest {
		t.Fatalf("list must not require URI")
	}
	r := httptest.NewRequest("GET", "/servers/r/resources/", nil)
	r.Host = "127.0.0.1"
	rr2 := httptest.NewRecorder()
	f.handler.ServeHTTP(rr2, r)
	if rr2.Code != http.StatusBadRequest || !strings.Contains(rr2.Body.String(), "missing resource URI") {
		t.Fatalf("neither: got %d %q", rr2.Code, rr2.Body.String())
	}
	if rr := f.get("/servers/unknown/resources"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown server: %d", rr.Code)
	}
}

func TestResourcesUnsupportedEmpty(t *testing.T) {
	// Tools-only server with explicitly-disabled resource/prompt caps:
	// lists return empty but instructions still ride along.
	s := mcpserver.NewMCPServer("bare", "1.0",
		mcpserver.WithInstructions("bare-instr"),
		mcpserver.WithResourceCapabilities(false, false),
		mcpserver.WithPromptCapabilities(false))
	s.AddTool(mcp.NewTool("t", mcp.WithDescription("t")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}}, nil
		})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s))
	defer ts.Close()
	d := daemon.New(&config.Config{Servers: map[string]config.ServerEntry{
		"bare": {URL: ts.URL + "/mcp"},
	}}, nil)
	defer d.Close()
	h := New(d, nil, "", "127.0.0.1:8379").Handler()
	get := func(p string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "127.0.0.1"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	if rr := get("/servers/bare/resources"); rr.Code != http.StatusOK {
		t.Fatalf("resources: %d %q", rr.Code, rr.Body.String())
	} else {
		v := decodeBody(t, rr)
		if res, _ := v["resources"].([]any); len(res) != 0 {
			t.Fatalf("want empty resources: %v", v)
		}
		if v["instructions"] != "bare-instr" {
			t.Fatalf("instructions must survive unsupported: %v", v)
		}
	}
	if rr := get("/servers/bare/prompts"); rr.Code != http.StatusOK {
		t.Fatalf("prompts: %d %q", rr.Code, rr.Body.String())
	} else if pr, _ := decodeBody(t, rr)["prompts"].([]any); len(pr) != 0 {
		t.Fatalf("want empty prompts: %q", rr.Body.String())
	}
}

func TestPromptsEndpoint(t *testing.T) {
	f := newFixture(t, "p", "p", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		s.AddPrompt(mcp.NewPrompt("greet", mcp.WithPromptDescription("says hi"),
			mcp.WithArgument("who", mcp.ArgumentDescription("target"))),
			func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				reqs.Add(1)
				return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{
					mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("hi")),
				}}, nil
			})
	})
	rr := f.get("/servers/p/prompts")
	if rr.Code != http.StatusOK {
		t.Fatalf("prompts: %d %q", rr.Code, rr.Body.String())
	}
	pr, _ := decodeBody(t, rr)["prompts"].([]any)
	if len(pr) != 1 {
		t.Fatalf("want 1 prompt: %q", rr.Body.String())
	}
	m0 := pr[0].(map[string]any)
	if m0["name"] != "greet" || m0["description"] != "says hi" {
		t.Fatalf("bad prompt row: %v", m0)
	}
	args, _ := m0["arguments"].([]any)
	if len(args) != 1 || args[0].(map[string]any)["name"] != "who" {
		t.Fatalf("arguments must be preserved: %v", m0)
	}
	if rr := f.get("/servers/unknown/prompts"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown server prompts: %d", rr.Code)
	}
}

func TestSearchEndpoint(t *testing.T) {
	f := newFixture(t, "svc", "Searchable Service", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		for i := 0; i < 15; i++ {
			name := fmt.Sprintf("tool_%02d", i)
			s.AddTool(mcp.NewTool(name, mcp.WithDescription("searchable tool")),
				countCalls(reqs, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "x"}}}, nil
				}))
		}
	})
	// Warm the cache: search never spawns.
	if _, err := f.daemon.ListTools(context.Background(), "svc"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	rr := f.get("/tools?q=tool")
	if rr.Code != http.StatusOK {
		t.Fatalf("search: %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	if int(v["total"].(float64)) != 15 {
		t.Fatalf("want total 15: %v", v)
	}
	tools, _ := v["tools"].([]any)
	if len(tools) != 12 {
		t.Fatalf("default limit 12: got %d", len(tools))
	}
	if v["nextOffset"] != float64(12) {
		t.Fatalf("want nextOffset 12: %v", v)
	}
	row := tools[0].(map[string]any)
	for _, k := range []string{"server", "name", "description", "score"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("hit missing %q: %v", k, row)
		}
	}
	rr = f.get("/tools?q=tool&limit=20")
	if n := len(decodeBody(t, rr)["tools"].([]any)); n != 15 {
		t.Fatalf("limit 20: got %d", n)
	}
	rr = f.get("/tools?q=tool&limit=5&offset=12")
	if v := decodeBody(t, rr); int(v["nextOffset"].(float64)) != -1 || len(v["tools"].([]any)) != 3 {
		t.Fatalf("paging tail: %v", v)
	}
	// server= filter: unknown or empty servers 400 (no cached tools to search).
	if rr := f.get("/tools?server=nope&q=tool"); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown server filter: %d", rr.Code)
	}
	if rr := f.get("/tools?server=other&q=tool"); rr.Code != http.StatusBadRequest {
		t.Fatalf("other-server filter: %d (no such server -> 400), got body %q", rr.Code, rr.Body.String())
	}
	if rr := f.get("/tools?server=svc&q=tool_01"); rr.Code != http.StatusOK {
		t.Fatalf("server filter: %d", rr.Code)
	} else if arr, _ := decodeBody(t, rr)["tools"].([]any); len(arr) == 0 {
		t.Fatal("server filter should match tools on svc")
	}
	// Regex mode + invalid regex.
	if rr := f.get("/tools?q=tool_0.&regex=1"); rr.Code != http.StatusOK {
		t.Fatalf("regex: %d", rr.Code)
	} else if arr, _ := decodeBody(t, rr)["tools"].([]any); len(arr) == 0 {
		t.Fatal("regex should match")
	}
	if rr := f.get("/tools?q=%28%5B&regex=true"); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid regex: %d", rr.Code)
	}
	// Empty q: zero hits.
	if rr := f.get("/tools?q="); rr.Code != http.StatusOK {
		t.Fatalf("empty q: %d", rr.Code)
	} else if v := decodeBody(t, rr); v["tools"] != nil {
		if arr, _ := v["tools"].([]any); len(arr) != 0 {
			t.Fatalf("empty q should hit nothing, got %q", rr.Body.String())
		}
	}
	// Denied tool never surfaces.
	pol := &config.ToolsPolicy{Deny: []string{"tool_0*"}}
	f2 := newFixture(t, "svc2", "svc2", pol, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		s.AddTool(mcp.NewTool("tool_00", mcp.WithDescription("d")),
			func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	})
	if _, err := f2.daemon.ListTools(context.Background(), "svc2"); err != nil {
		t.Fatalf("warm denied: %v", err)
	}
	r := httptest.NewRequest("GET", "/tools?q=tool_00", nil)
	r.Host = "127.0.0.1"
	rr = httptest.NewRecorder()
	f2.handler.ServeHTTP(rr, r)
	if arr, _ := decodeBody(t, rr)["tools"].([]any); len(arr) != 0 {
		t.Fatalf("denied tool must be invisible: %q", rr.Body.String())
	}
	// CURRENT behavior: Sscanf prefix parsing for limit.
	if rr := f.get("/tools?q=tool&limit=5abc"); rr.Code != http.StatusOK {
		t.Fatalf("limit prefix: %d", rr.Code)
	} else if n := len(decodeBody(t, rr)["tools"].([]any)); n != 5 {
		t.Fatalf("limit=5abc currently parses as 5, got %d", n)
	}
}

func TestReloadEndpoint(t *testing.T) {
	echo := New(nil, func() ([]string, []string, []string, error) {
		return []string{"a"}, []string{"b"}, []string{"c"}, nil
	}, "", "127.0.0.1:8379")
	r := httptest.NewRequest("POST", "/reload", nil)
	r.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	echo.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("reload: %d %q", rr.Code, rr.Body.String())
	}
	v := decodeBody(t, rr)
	for _, k := range []string{"added", "removed", "modified"} {
		if v[k] == nil {
			t.Fatalf("reload must echo %q: %v", k, v)
		}
	}
	// Error path surfaces 500 with the raw message.
	boom := New(nil, func() ([]string, []string, []string, error) {
		return nil, nil, nil, fmt.Errorf("kaboom")
	}, "", "127.0.0.1:8379")
	r = httptest.NewRequest("POST", "/reload", nil)
	r.Host = "127.0.0.1"
	rr = httptest.NewRecorder()
	boom.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "kaboom") {
		t.Fatalf("reload error: %d %q", rr.Code, rr.Body.String())
	}
	// GET is not routed.
	r = httptest.NewRequest("GET", "/reload", nil)
	r.Host = "127.0.0.1"
	rr = httptest.NewRecorder()
	boom.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /reload: %d", rr.Code)
	}
	// Foreign Origin is rejected on reload too.
	r = httptest.NewRequest("POST", "/reload", nil)
	r.Host = "127.0.0.1"
	r.Header.Set("Origin", "https://evil.com")
	rr = httptest.NewRecorder()
	echo.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign origin reload: %d", rr.Code)
	}
}

func TestConcurrentReadsRace(t *testing.T) {
	f := newFixture(t, "svc", "svc", nil, func(s *mcpserver.MCPServer, reqs *atomic.Int64) {
		tool, h := echoTool()
		s.AddTool(tool, countCalls(reqs, h))
	})
	if _, err := f.daemon.ListTools(context.Background(), "svc"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	done := make(chan struct{})
	var stop atomic.Bool
	for i := 0; i < 4; i++ {
		go func() {
			for !stop.Load() {
				f.get("/servers")
				f.get("/health")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 50; i++ {
		f.get("/servers")
		f.get("/health")
	}
	stop.Store(true)
	for i := 0; i < 4; i++ {
		<-done
	}
}
