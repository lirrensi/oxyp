package peerfederation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// Proxy is a client.MCPClient over one peer server's native REST API.
// It lets peer virtual servers ride the daemon's existing spawn/clientFor,
// cache, health, and sweep paths unchanged.
//
// The REST client rides behind an atomic pointer: when the daemon rebuilds
// the peer connection (URL/token/TLS change), it swaps the client and every
// live Proxy follows — no stale host, no stale token, no split brain.
type Proxy struct {
	client atomic.Pointer[Client]
	server string // owner-side server name
	peer   string // peer name (for errors/attribution)
	v1     atomic.Bool
	mu     sync.Mutex
	notif  func(mcp.JSONRPCNotification)
}

var _ client.MCPClient = (*Proxy)(nil)

// NewProxy binds a peer client to one owner-side server.
// v1 selects /federation/v1 routes (bulk); false uses thin REST.
// A v1 proxy that 404s self-heals down to thin (owner downgraded).
func NewProxy(c *Client, peer, server string, v1 bool) *Proxy {
	p := &Proxy{server: server, peer: peer}
	p.client.Store(c)
	p.v1.Store(v1)
	return p
}

// Rebind swaps the underlying REST client (peer connection rebuild).
// Safe for concurrent use; in-flight calls finish on the old client.
func (p *Proxy) Rebind(c *Client) {
	if c != nil {
		p.client.Store(c)
	}
}

// rest returns the current REST client.
func (p *Proxy) rest() *Client {
	return p.client.Load()
}

func (p *Proxy) Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	h, err := p.rest().Health(ctx)
	if err != nil {
		return nil, p.wrap(err)
	}
	res := &mcp.InitializeResult{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ServerInfo:      mcp.Implementation{Name: "oxyp-peer:" + p.peer, Version: "0.1.0"},
		Capabilities: mcp.ServerCapabilities{Tools: &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{}},
	}
	// Instructions ride the sync catalog (one fetch per peer per sync),
	// not a second fetch per spawn: Initialize stays one /health.
	_ = h
	return res, nil
}

func (p *Proxy) Ping(ctx context.Context) error {
	h, err := p.rest().Health(ctx)
	if err != nil {
		return p.wrap(err)
	}
	if h.Servers != nil {
		if sh, ok := h.Servers[p.server]; ok && sh.Error != "" && (sh.Healthy == nil || !*sh.Healthy) {
			return fmt.Errorf("peer %q server %q: %s", p.peer, p.server, sh.Error)
		}
	}
	return nil
}

func (p *Proxy) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return p.ListToolsByPage(ctx, req)
}

func (p *Proxy) ListToolsByPage(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	// v1: one catalog fetch carries full schemas (no N+1 describes).
	// Downgrade-on-404 matches the prompt/template siblings.
	if p.v1.Load() {
		cat, err := p.rest().Catalog(ctx)
		if err == nil {
			for _, s := range cat.Servers {
				if s.Name == p.server {
					tools := make([]mcp.Tool, 0, len(s.Tools))
					for _, ct := range s.Tools {
						tools = append(tools, ct.ToTool())
					}
					sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
					return &mcp.ListToolsResult{Tools: tools}, nil
				}
			}
			return &mcp.ListToolsResult{}, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrap(err)
		}
		p.v1.Store(false)
	}
	stubs, err := p.rest().Tools(ctx, p.server)
	if err != nil {
		return nil, p.wrap(err)
	}
	tools := make([]mcp.Tool, 0, len(stubs))
	for _, s := range stubs {
		t := mcp.Tool{Name: s.Name, Description: s.Description}
		if d, derr := p.rest().Describe(ctx, p.server, s.Name); derr == nil && d != nil {
			t.Description = d.Description
			if len(d.InputSchema) != 0 {
				if raw, merr := json.Marshal(d.InputSchema); merr == nil {
					t.RawInputSchema = raw
					// Hydrate the structured form too: daemon describe
					// paths and cache round-trips read InputSchema.
					_ = json.Unmarshal(raw, &t.InputSchema)
				}
			}
		} else if derr != nil {
			// A single undescribable tool must not poison the list;
			// keep the stub (name+description) and move on.
			_ = derr
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return &mcp.ListToolsResult{Tools: tools}, nil
}

func (p *Proxy) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args map[string]any
	switch a := req.Params.Arguments.(type) {
	case nil:
	case map[string]any:
		args = a
	default:
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		args = map[string]any{}
		if err := json.Unmarshal(b, &args); err != nil {
			return nil, err
		}
	}
	var meta map[string]any
	if req.Params.Meta != nil {
		if raw, err := json.Marshal(req.Params.Meta); err == nil {
			_ = json.Unmarshal(raw, &meta)
		}
	}
	if p.v1.Load() {
		res, err := p.rest().VCall(ctx, p.server, req.Params.Name, args, meta)
		if err == nil {
			return res, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrapCall(req.Params.Name, err)
		}
		p.v1.Store(false) // owner lost v1 (downgrade): thin from here
	}
	res, err := p.rest().Call(ctx, p.server, req.Params.Name, args)
	if err != nil {
		return nil, p.wrapCall(req.Params.Name, err)
	}
	return res, nil
}

func (p *Proxy) ListResources(ctx context.Context, req mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error) {
	return p.ListResourcesByPage(ctx, req)
}

func (p *Proxy) ListResourcesByPage(ctx context.Context, req mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error) {
	stubs, _, err := p.rest().Resources(ctx, p.server)
	if err != nil {
		return nil, p.wrap(err)
	}
	out := &mcp.ListResourcesResult{}
	for _, s := range stubs {
		out.Resources = append(out.Resources, mcp.Resource{
			URI: s.URI, Name: s.Name, Description: s.Description, MIMEType: s.MimeType,
		})
	}
	return out, nil
}

func (p *Proxy) ListResourceTemplates(ctx context.Context, req mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error) {
	return p.ListResourceTemplatesByPage(ctx, req)
}

func (p *Proxy) ListResourceTemplatesByPage(ctx context.Context, req mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error) {
	if p.v1.Load() {
		cat, err := p.rest().Catalog(ctx)
		if err == nil {
			for _, s := range cat.Servers {
				if s.Name == p.server {
					return &mcp.ListResourceTemplatesResult{ResourceTemplates: s.Templates}, nil
				}
			}
			return &mcp.ListResourceTemplatesResult{}, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrap(err)
		}
		p.v1.Store(false)
	}
	return nil, fmt.Errorf("%w: peer %q has no resource templates", mcp.ErrMethodNotFound, p.peer)
}

func (p *Proxy) ReadResource(ctx context.Context, req mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if p.v1.Load() {
		res, err := p.rest().VResource(ctx, p.server, req.Params.URI)
		if err == nil {
			return res, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrap(err)
		}
		p.v1.Store(false)
	}
	maps, err := p.rest().ReadResource(ctx, p.server, req.Params.URI)
	if err != nil {
		return nil, p.wrap(err)
	}
	res := &mcp.ReadResourceResult{}
	for _, cm := range maps {
		rc, err := mcp.ParseResourceContents(cm)
		if err != nil {
			return nil, fmt.Errorf("peer %q read %q: %w", p.peer, req.Params.URI, err)
		}
		res.Contents = append(res.Contents, rc)
	}
	return res, nil
}

func (p *Proxy) Subscribe(ctx context.Context, req mcp.SubscribeRequest) error {
	return fmt.Errorf("%w: peer %q has no subscriptions", mcp.ErrMethodNotFound, p.peer)
}

func (p *Proxy) Unsubscribe(ctx context.Context, req mcp.UnsubscribeRequest) error {
	return fmt.Errorf("%w: peer %q has no subscriptions", mcp.ErrMethodNotFound, p.peer)
}

func (p *Proxy) ListPrompts(ctx context.Context, req mcp.ListPromptsRequest) (*mcp.ListPromptsResult, error) {
	return p.ListPromptsByPage(ctx, req)
}

func (p *Proxy) ListPromptsByPage(ctx context.Context, req mcp.ListPromptsRequest) (*mcp.ListPromptsResult, error) {
	if p.v1.Load() {
		cat, err := p.rest().Catalog(ctx)
		if err == nil {
			for _, s := range cat.Servers {
				if s.Name == p.server {
					return &mcp.ListPromptsResult{Prompts: s.Prompts}, nil
				}
			}
			return &mcp.ListPromptsResult{}, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrap(err)
		}
		p.v1.Store(false)
	}
	// Thin REST lists templates but cannot fetch them (no GetPrompt
	// endpoint) — advertising would lie. Empty until v1.
	return &mcp.ListPromptsResult{}, nil
}

func (p *Proxy) GetPrompt(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	if p.v1.Load() {
		res, err := p.rest().VPrompt(ctx, p.server, req.Params.Name, req.Params.Arguments)
		if err == nil {
			return res, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrap(err)
		}
		p.v1.Store(false)
	}
	return nil, fmt.Errorf("%w: peer %q has no prompt fetch", mcp.ErrMethodNotFound, p.peer)
}

func (p *Proxy) SetLevel(ctx context.Context, req mcp.SetLevelRequest) error { return nil }

func (p *Proxy) Complete(ctx context.Context, req mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	if p.v1.Load() {
		res, err := p.rest().VComplete(ctx, p.server, req.Params)
		if err == nil {
			return res, nil
		}
		if !IsNotFound(err) {
			return nil, p.wrap(err)
		}
		p.v1.Store(false)
	}
	return nil, fmt.Errorf("%w: peer %q has no completion", mcp.ErrMethodNotFound, p.peer)
}

func (p *Proxy) Close() error { return nil }

func (p *Proxy) OnNotification(handler func(notification mcp.JSONRPCNotification)) {
	p.mu.Lock()
	p.notif = handler
	p.mu.Unlock()
}

// wrap maps peer failures into typed errors the daemon already handles:
// owner 401 (downstream OAuth) → auth-required; owner policy/param 403 →
// policy errors; everything else stays a 502-class error with attribution.
func (p *Proxy) wrap(err error) error {
	pe, ok := err.(*PeerError)
	if !ok {
		return fmt.Errorf("peer %q server %q: %w", p.peer, p.server, err)
	}
	return p.wrapPeer(pe, "")
}

// wrapCall maps call failures; tool name rides for param/policy errors.
func (p *Proxy) wrapCall(tool string, err error) error {
	pe, ok := err.(*PeerError)
	if !ok {
		return fmt.Errorf("peer %q server %q tool %q: %w", p.peer, p.server, tool, err)
	}
	return p.wrapPeer(pe, tool)
}

func (p *Proxy) wrapPeer(pe *PeerError, tool string) error {
	switch {
	case pe.Status == httpStatusUnauthorized && pe.AuthRequired != "":
		// Owner-side downstream OAuth: the login must happen ON THE
		// OWNER. Surface the peer-qualified hint; local login refuses.
		return &AuthNeededError{Peer: p.peer, Server: pe.Server, Hint: pe.AuthRequired}
	case pe.Status == httpStatusUnauthorized:
		return fmt.Errorf("peer %q: unauthorized (check peers.%s.token)", p.peer, p.peer)
	case pe.Status == httpStatusForbidden && pe.Policy == "param-blocked":
		return &ParamDeniedError{Server: p.qualified(), Tool: tool, Allowed: pe.Allowed, Message: pe.Err}
	case pe.Status == httpStatusForbidden && pe.Policy == "blocked":
		return &PolicyDeniedError{Server: p.qualified(), Tool: tool, Message: pe.Err}
	case pe.Status == httpStatusNotFound:
		return fmt.Errorf("peer %q server %q: %s", p.peer, p.server, firstNonEmpty(pe.Err, "not found"))
	default:
		if pe.Err != "" {
			return fmt.Errorf("peer %q server %q: %s", p.peer, p.server, pe.Err)
		}
		return fmt.Errorf("peer %q server %q: status %d", p.peer, p.server, pe.Status)
	}
}

func (p *Proxy) qualified() string {
	return p.peer + ":" + p.server
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// HTTP status codes, spelled out to avoid importing net/http here.
const (
	httpStatusUnauthorized = 401
	httpStatusForbidden    = 403
	httpStatusNotFound     = 404
)
