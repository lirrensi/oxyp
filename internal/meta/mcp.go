package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/lirrensi/oxyp/internal/outguard"
	peerfederation "github.com/lirrensi/oxyp/internal/peerfederation"
	"github.com/lirrensi/oxyp/internal/perms"
)

// mcpGateway builds the Streamable-HTTP MCP gateway: the same 5 meta-tools
// the stdio shim speaks, served over POST /mcp for URL-only clients.
// Discovery tools return JSON-in-text; call forwards downstream content
// blocks verbatim (media passthrough). Per-client scoping comes from tool
// args (group/servers/exclude) over POST /mcp?group=&servers=&exclude= over
// OXYP_GROUP/OXYP_SERVERS/OXYP_EXCLUDE env — see gatewayScope.
func (s *Server) mcpGateway() *mcpserver.MCPServer {
	gw := mcpserver.NewMCPServer("oxyp", "0.3.0",
		mcpserver.WithInstructions("oxyp lazy MCP gateway: search_tools → describe_tool → call. Schemas on demand, never guess."),
		mcpserver.WithToolCapabilities(false),
	)
	gw.AddTool(
		mcp.NewTool("list_servers",
			mcp.WithDescription("List downstream MCP servers. Optional filter matches name/description. Optional group/servers/exclude narrow the client view (servers/exclude are comma-separated exact-or-glob IDs, deny wins)."),
			mcp.WithString("filter", mcp.Description("Substring filter over name/description.")),
			mcp.WithString("group", mcp.Description("Curated subset ([groups.*]). Unknown group errors.")),
			mcp.WithString("servers", mcp.Description("Whitelist: only these servers (comma-separated exact-or-glob IDs, e.g. github,wsl:*). Empty = all.")),
			mcp.WithString("exclude", mcp.Description("Blacklist: hide these servers (comma-separated exact-or-glob IDs, deny wins).")),
		),
		s.gwListServers,
	)
	gw.AddTool(
		mcp.NewTool("list_tools",
			mcp.WithDescription("Thin tool list (name+description) for one server."),
			mcp.WithString("server", mcp.Description("Server name."), mcp.Required()),
			mcp.WithString("group", mcp.Description("Curated subset; server outside it is forbidden.")),
			mcp.WithString("servers", mcp.Description("Whitelist: only these servers (comma-separated exact-or-glob IDs).")),
			mcp.WithString("exclude", mcp.Description("Blacklist: hide these servers (comma-separated exact-or-glob IDs, deny wins).")),
		),
		s.gwListTools,
	)
	gw.AddTool(
		mcp.NewTool("describe_tool",
			mcp.WithDescription("Full schemas for named tools on one server."),
			mcp.WithString("server", mcp.Description("Server name."), mcp.Required()),
			mcp.WithString("group", mcp.Description("Curated subset; server outside it is forbidden.")),
			mcp.WithString("servers", mcp.Description("Whitelist: only these servers (comma-separated exact-or-glob IDs).")),
			mcp.WithString("exclude", mcp.Description("Blacklist: hide these servers (comma-separated exact-or-glob IDs, deny wins).")),
			mcp.WithArray("tools", mcp.Description("Tool names to describe."),
				mcp.WithStringItems(mcp.Description("Tool name."))),
		),
		s.gwDescribe,
	)
	gw.AddTool(
		mcp.NewTool("call",
			mcp.WithDescription("Call a downstream tool. Args nest under parameters."),
			mcp.WithString("server", mcp.Description("Server name."), mcp.Required()),
			mcp.WithString("tool", mcp.Description("Tool name."), mcp.Required()),
			mcp.WithString("group", mcp.Description("Curated subset; server outside it is forbidden.")),
			mcp.WithString("servers", mcp.Description("Whitelist: only these servers (comma-separated exact-or-glob IDs).")),
			mcp.WithString("exclude", mcp.Description("Blacklist: hide these servers (comma-separated exact-or-glob IDs, deny wins).")),
			mcp.WithObject("parameters", mcp.Description("Tool arguments.")),
		),
		s.gwCall,
	)
	gw.AddTool(
		mcp.NewTool("search_tools",
			mcp.WithDescription("Ranked tool search across servers. Never spawns; searches cached metadata only."),
			mcp.WithString("query", mcp.Description("Space-separated search words."), mcp.Required()),
			mcp.WithString("server", mcp.Description("Restrict to one server.")),
			mcp.WithString("group", mcp.Description("Curated subset.")),
			mcp.WithString("servers", mcp.Description("Whitelist: only these servers (comma-separated exact-or-glob IDs).")),
			mcp.WithString("exclude", mcp.Description("Blacklist: hide these servers (comma-separated exact-or-glob IDs, deny wins).")),
			mcp.WithBoolean("regex", mcp.Description("Query is a Go regexp (unranked).")),
			mcp.WithNumber("limit", mcp.Description("Page size (default 12).")),
			mcp.WithNumber("offset", mcp.Description("Page offset.")),
		),
		s.gwSearch,
	)
	return gw
}

// gatewayQueryKey carries POST /mcp?group=&servers=&exclude= into tool
// handlers (mcp-go hides the HTTP request from handlers; the context func
// stashes the query layer here).
type gatewayQueryKey struct{}

func withGatewayQuery(ctx context.Context, scope daemon.Scope) context.Context {
	return context.WithValue(ctx, gatewayQueryKey{}, scope)
}

func gatewayQueryScope(ctx context.Context) daemon.Scope {
	if scope, ok := ctx.Value(gatewayQueryKey{}).(daemon.Scope); ok {
		return scope
	}
	return daemon.Scope{}
}

// gatewayGroup resolves group scoping for a gateway call: explicit arg wins,
// POST /mcp?group= is next, OXYP_GROUP env is the default (shim parity: one
// group per client).
func gatewayGroup(arg string) string {
	if arg != "" {
		return arg
	}
	return gatewayEnvGroup()
}

func gatewayEnvGroup() string {
	return strings.TrimSpace(os.Getenv("OXYP_GROUP"))
}

func gatewayEnvAllow() []string {
	return daemon.ParseList(os.Getenv("OXYP_SERVERS"))
}

func gatewayEnvDeny() []string {
	return daemon.ParseList(os.Getenv("OXYP_EXCLUDE"))
}

// gatewayScope resolves the full per-call view: each dimension (group,
// servers, exclude) picks explicit tool arg over POST /mcp?query over env.
// Presence wins per dimension: an explicit arg (even "") overrides query/env,
// so an agent can narrow but clearing env needs an explicit empty.
func gatewayScope(ctx context.Context, args map[string]any) daemon.Scope {
	q := gatewayQueryScope(ctx)
	var scope daemon.Scope
	if v, ok := args["group"]; ok {
		scope.Group = gwStr(args, "group")
		_ = v
	} else if q.Group != "" {
		scope.Group = q.Group
	} else {
		scope.Group = gatewayEnvGroup()
	}
	if _, ok := args["servers"]; ok {
		scope.Allow = daemon.ParseArgList(args["servers"])
	} else if len(q.Allow) != 0 {
		scope.Allow = q.Allow
	} else {
		scope.Allow = gatewayEnvAllow()
	}
	if _, ok := args["exclude"]; ok {
		scope.Deny = daemon.ParseArgList(args["exclude"])
	} else if len(q.Deny) != 0 {
		scope.Deny = q.Deny
	} else {
		scope.Deny = gatewayEnvDeny()
	}
	return scope
}

// gatewayScopeError maps scope violations to gateway errors: unknown group,
// else forbidden with the legacy/scope message split (REST parity).
func (s *Server) gatewayScopeError(scope daemon.Scope, srv string) error {
	if scope.Group != "" {
		if _, ok := s.d.GroupMembers(scope.Group); !ok {
			return fmt.Errorf("group not found: %q", scope.Group)
		}
	}
	if !s.d.InScope(scope, srv) {
		if len(scope.Allow) == 0 && len(scope.Deny) == 0 {
			return fmt.Errorf("server %q not in group %q", srv, scope.Group)
		}
		return fmt.Errorf("server %q not in scope", srv)
	}
	return nil
}

func (s *Server) gwErr(err error) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "oxyp: " + err.Error()}},
		IsError: true,
	}, nil
}

func gwText(v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}},
	}
}

func gwStr(args map[string]any, k string) string {
	if s, ok := args[k].(string); ok {
		return s
	}
	return ""
}

func gwNum(args map[string]any, k string) int {
	switch n := args[k].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func (s *Server) gwListServers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := req.Params.Arguments.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	scope := gatewayScope(ctx, args)
	filter := strings.ToLower(gwStr(args, "filter"))
	if scope.Group != "" {
		if _, ok := s.d.GroupMembers(scope.Group); !ok {
			return s.gwErr(fmt.Errorf("group not found: %q", scope.Group))
		}
	}
	type info struct {
		Name     string   `json:"name"`
		Desc     string   `json:"description"`
		Origin   string   `json:"origin,omitempty"`
		Shadowed []string `json:"shadowed,omitempty"`
	}
	peerView := peerfederation.IsPeer(ctx)
	names := s.d.Names()
	if peerView {
		names = s.d.PeerNames()
	}
	var out []info
	for _, name := range names {
		if !s.d.InScope(scope, name) {
			continue
		}
		if peerView && !s.d.SharedWithPeer(name) {
			continue
		}
		e := s.d.Entry(name)
		if e == nil {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) &&
			!strings.Contains(strings.ToLower(e.Description), filter) {
			continue
		}
		origin := "local"
		if e.Peer != "" {
			origin = e.Peer
		}
		out = append(out, info{name, e.Description, origin, s.d.Shadows(name)})
	}
	if out == nil {
		out = []info{}
	}
	return gwText(map[string]any{"servers": out}), nil
}

func (s *Server) gwListTools(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := req.Params.Arguments.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	srv := gwStr(args, "server")
	if srv == "" {
		return s.gwErr(fmt.Errorf("server required"))
	}
	scope := gatewayScope(ctx, args)
	if err := s.gatewayScopeError(scope, srv); err != nil {
		return s.gwErr(err)
	}
	e := s.d.Entry(srv)
	if e == nil || (peerfederation.IsPeer(ctx) && !s.d.SharedWithPeer(srv)) {
		return s.gwErr(fmt.Errorf("server not found: %q", srv))
	}
	tools, err := s.d.ListTools(ctx, srv)
	if err != nil {
		return s.gwErr(err)
	}
	type stub struct {
		Name string `json:"name"`
		Desc string `json:"description"`
	}
	out := []stub{}
	for _, t := range tools {
		if perms.Resolve(e, t.Name).Action != "allow" {
			continue
		}
		out = append(out, stub{t.Name, t.Description})
	}
	return gwText(map[string]any{"tools": out}), nil
}

func (s *Server) gwDescribe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := req.Params.Arguments.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	srv := gwStr(args, "server")
	if srv == "" {
		return s.gwErr(fmt.Errorf("server required"))
	}
	scope := gatewayScope(ctx, args)
	if err := s.gatewayScopeError(scope, srv); err != nil {
		return s.gwErr(err)
	}
	e := s.d.Entry(srv)
	if e == nil || (peerfederation.IsPeer(ctx) && !s.d.SharedWithPeer(srv)) {
		return s.gwErr(fmt.Errorf("server not found: %q", srv))
	}
	var names []string
	if raw, ok := args["tools"].([]any); ok {
		for _, n := range raw {
			if ns, _ := n.(string); ns != "" {
				names = append(names, ns)
			}
		}
	}
	tools, err := s.d.ListTools(ctx, srv)
	if err != nil {
		return s.gwErr(err)
	}
	byName := map[string]mcp.Tool{}
	for _, t := range tools {
		byName[strings.ToLower(t.Name)] = t
	}
	var out []any
	for _, n := range names {
		// deny ⇒ invisible: same 404 shape as unknown.
		if perms.Resolve(e, n).Action != "allow" {
			return s.gwErr(fmt.Errorf("tool not found: %q", n))
		}
		t, ok := byName[strings.ToLower(n)]
		if !ok {
			return s.gwErr(fmt.Errorf("tool not found: %q", n))
		}
		out = append(out, map[string]any{
			"name": t.Name, "description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return gwText(map[string]any{"schemas": out}), nil
}

func (s *Server) gwSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := req.Params.Arguments.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	q := gwStr(args, "query")
	if q == "" {
		return s.gwErr(fmt.Errorf("query required"))
	}
	scope := gatewayScope(ctx, args)
	if scope.Group != "" {
		if _, ok := s.d.GroupMembers(scope.Group); !ok {
			return s.gwErr(fmt.Errorf("group not found: %q", scope.Group))
		}
	}
	srv := gwStr(args, "server")
	if peerfederation.IsPeer(ctx) && srv != "" && !s.d.SharedWithPeer(srv) {
		return s.gwErr(fmt.Errorf("server not found: %q", srv))
	}
	regex, _ := args["regex"].(bool)
	hits, total, next, err := s.d.SearchTools(srv, q, regex, gwNum(args, "limit"), gwNum(args, "offset"))
	if err != nil {
		return s.gwErr(err)
	}
	peerView := peerfederation.IsPeer(ctx)
	out := []map[string]any{}
	for _, h := range hits {
		if !s.d.InScope(scope, h.Server) {
			continue
		}
		if peerView && !s.d.SharedWithPeer(h.Server) {
			continue
		}
		out = append(out, map[string]any{
			"server": h.Server, "name": h.Name,
			"description": h.Description, "score": h.Score,
		})
	}
	return gwText(map[string]any{"tools": out, "total": total, "nextOffset": next}), nil
}

func (s *Server) gwCall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := req.Params.Arguments.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	srv, tool := gwStr(args, "server"), gwStr(args, "tool")
	if srv == "" || tool == "" {
		return s.gwErr(fmt.Errorf("server and tool required"))
	}
	scope := gatewayScope(ctx, args)
	if err := s.gatewayScopeError(scope, srv); err != nil {
		return s.gwErr(err)
	}
	e := s.d.Entry(srv)
	if e == nil || (peerfederation.IsPeer(ctx) && !s.d.SharedWithPeer(srv)) {
		return s.gwErr(fmt.Errorf("server not found: %q", srv))
	}
	if dec := perms.Resolve(e, tool); dec.Action != "allow" {
		return s.gwErr(fmt.Errorf("blocked by policy: %s", dec.Message))
	}
	params, _ := args["parameters"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	res, err := s.d.CallTool(ctx, srv, tool, params)
	if err != nil {
		return s.gwErr(err)
	}
	// Guard text only; media blocks pass verbatim (shim parity).
	content := res.Content
	if g := e.EffectiveGuard(s.d.DaemonConfig()); g != nil {
		content, _ = outguard.Apply(content, outguard.Limits{
			MaxBytes: g.MaxBytes, MaxLines: g.MaxLines,
		})
	}
	out := &mcp.CallToolResult{Content: content, IsError: res.IsError}
	if res.StructuredContent != nil {
		out.StructuredContent = res.StructuredContent
	}
	if res.Meta != nil {
		out.Meta = res.Meta
	}
	return out, nil
}
