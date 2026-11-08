package peerfederation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/remhttp"
)

// Federation headers and limits.
const (
	// HeaderInstance carries the sender's daemon instance id.
	HeaderInstance = "X-Oxyp-Instance"
	// HeaderVisited carries the instance-id chain (comma-separated).
	HeaderVisited = "X-Oxyp-Visited"
	// MaxVisited caps the federation chain length (loop bound).
	MaxVisited = 8
	// MaxBody bounds peer response bodies (64 MiB, same as invoke cap).
	MaxBody = 64 << 20
)

// Server is one peer-side server listing (GET /servers item).
type Server struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Healthy      *bool  `json:"healthy"`
	Error        string `json:"error"`
	AuthRequired string `json:"auth_required"`
	Connected    bool   `json:"connected"`
	Transitive   bool   `json:"transitive"`
}

// ToolStub is one peer-side tool listing (GET .../tools item).
type ToolStub struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ToolSchema is one peer-side describe response (GET .../tools/{tool}).
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ResourceStub is one peer-side resource listing item.
type ResourceStub struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// PromptStub is one peer-side prompt listing item.
type PromptStub struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Arguments   []argStub `json:"arguments"`
}

type argStub struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// Health mirrors the peer's GET /health per-server entries.
type Health struct {
	PID       int                     `json:"pid"`
	StartedAt string                  `json:"startedAt"`
	Instance  string                  `json:"instance"`
	Servers   map[string]serverHealth `json:"servers"`
}

type serverHealth struct {
	Connected    bool   `json:"connected"`
	Healthy      *bool  `json:"healthy"`
	Error        string `json:"error"`
	AuthRequired string `json:"auth_required"`
}

// PeerError is a failed peer REST call with status + decoded body.
type PeerError struct {
	Status       int
	Err          string
	AuthRequired string
	Allowed      []string
	Policy       string
	Server       string
	Tool         string
}

func (e *PeerError) Error() string {
	if e.Err != "" {
		return fmt.Sprintf("peer %d: %s", e.Status, e.Err)
	}
	return fmt.Sprintf("peer %d", e.Status)
}

// Client speaks one peer daemon's native REST API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient builds a peer client. base is the peer URL (path/query/fragment
// stripped, like OXYP_URL handling); tls is the peer TLS config.
// NewClient builds a peer client. timeout is accepted for API stability
// but ignored: request contexts govern deadlines (see above).
func NewClient(base, token string, tlsCfg *config.TLSConfig, _ time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad peer url %q", base)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("bad peer url %q: scheme must be http(s)", base)
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
	var tt remhttp.TransportTLS
	if tlsCfg != nil {
		tt = remhttp.TransportTLS{
			CAFile: tlsCfg.CAFile, CertFile: tlsCfg.CertFile,
			KeyFile: tlsCfg.KeyFile, Insecure: tlsCfg.Insecure,
		}
	}
	// Timeout 0: request contexts govern (CallTool wraps effectiveTimeout;
	// sync paths set their own). An http.Client Timeout would cap slow tools
	// below the configured tool_timeout — same reason dialRemote uses a
	// Timeout:0 streamClient for non-peer remotes.
	hc := remhttp.NewClient(0, tt)
	// Federation headers must survive redirects; remhttp strips
	// everything but the MCP protocol set on cross-origin hops.
	// Peers are plain REST with no redirect contract — don't follow.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		base:  strings.TrimSuffix(u.String(), "/"),
		token: token,
		http:  hc,
	}, nil
}

func (c *Client) req(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// No Origin header: absent passes the owner's gate.
	if inst, ok := InstanceFrom(ctx); ok {
		req.Header.Set(HeaderInstance, inst)
	}
	if v := VisitedHeader(ctx); v != "" {
		req.Header.Set(HeaderVisited, v)
	}
	return req, nil
}

func (c *Client) do(req *http.Request, server, tool string) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("peer response exceeds %d bytes", MaxBody)
	}
	if resp.StatusCode < 400 {
		return b, nil
	}
	pe := &PeerError{Status: resp.StatusCode, Server: server, Tool: tool}
	var v struct {
		Err          string         `json:"error"`
		AuthRequired string         `json:"auth_required"`
		Allowed      []string       `json:"allowed"`
		Meta         map[string]any `json:"_meta"`
	}
	if jerr := json.Unmarshal(b, &v); jerr == nil {
		pe.Err = v.Err
		pe.AuthRequired = v.AuthRequired
		pe.Allowed = v.Allowed
		if p, _ := v.Meta["oxypPolicy"].(string); p != "" {
			pe.Policy = p
		}
	} else {
		pe.Err = strings.TrimSpace(string(b))
	}
	return nil, pe
}

// Servers lists the peer's servers (never spawns there).
func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	req, err := c.req(ctx, http.MethodGet, "/servers", nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, "", "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer /servers: %w", err)
	}
	return v.Servers, nil
}

// PeerInfo is the /peer handshake answer: identity + shared catalog preview.
type PeerInfo struct {
	Instance string   `json:"instance"`
	Name     string   `json:"name"`
	Servers  []string `json:"servers"`
	Count    int      `json:"count"`
}

// Peer performs the /peer handshake (identity + shared catalog preview).
func (c *Client) Peer(ctx context.Context) (*PeerInfo, error) {
	req, err := c.req(ctx, http.MethodGet, "/peer", nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, "", "")
	if err != nil {
		return nil, err
	}
	var v PeerInfo
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer /peer: %w", err)
	}
	return &v, nil
}

// CatalogTool is one bulk-catalog tool (full schema, owner-filtered).
type CatalogTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Title        string         `json:"title"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema"`
	Annotations  map[string]any `json:"annotations"`
	Icons        []mcp.Icon     `json:"icons"`
	Execution    map[string]any `json:"execution"`
	Meta         map[string]any `json:"_meta"`
}

// ToTool hydrates a native tool (raw schemas preserved + structured backfilled).
func (t CatalogTool) ToTool() mcp.Tool {
	tool := mcp.Tool{Name: t.Name, Description: t.Description, Title: t.Title}
	if len(t.InputSchema) != 0 {
		if raw, err := json.Marshal(t.InputSchema); err == nil {
			tool.RawInputSchema = raw
			_ = json.Unmarshal(raw, &tool.InputSchema)
		}
	}
	if len(t.OutputSchema) != 0 {
		if raw, err := json.Marshal(t.OutputSchema); err == nil {
			tool.RawOutputSchema = raw
			_ = json.Unmarshal(raw, &tool.OutputSchema)
		}
	}
	if len(t.Annotations) != 0 {
		if raw, err := json.Marshal(t.Annotations); err == nil {
			_ = json.Unmarshal(raw, &tool.Annotations)
		}
	}
	tool.Icons = t.Icons
	if len(t.Execution) != 0 {
		var ex mcp.ToolExecution
		if raw, err := json.Marshal(t.Execution); err == nil {
			if jerr := json.Unmarshal(raw, &ex); jerr == nil {
				tool.Execution = &ex
			}
		}
	}
	if t.Meta != nil {
		tool.Meta = mcp.NewMetaFromMap(t.Meta)
	}
	return tool
}

// CatalogServer is one bulk-catalog server row.
type CatalogServer struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Origin       string                 `json:"origin"`
	Instructions string                 `json:"instructions"`
	Healthy      *bool                  `json:"healthy"`
	Error        string                 `json:"error"`
	AuthRequired string                 `json:"auth_required"`
	Connected    bool                   `json:"connected"`
	Tools        []CatalogTool          `json:"tools"`
	Transitive   bool                   `json:"transitive"`
	Resources    []mcp.Resource         `json:"resources"`
	Templates    []mcp.ResourceTemplate `json:"templates"`
	Prompts      []mcp.Prompt           `json:"prompts"`
}

// Catalog is the /federation/v1/catalog answer.
type Catalog struct {
	Version  string          `json:"version"`
	Instance string          `json:"instance"`
	Name     string          `json:"name"`
	Rev      string          `json:"rev"`
	Servers  []CatalogServer `json:"servers"`
}

// Catalog fetches the bulk federation catalog (one round trip).
// Old peers (404) fall back to thin /servers + per-tool describes.
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	req, err := c.req(ctx, http.MethodGet, "/federation/v1/catalog", nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, "", "")
	if err != nil {
		return nil, err
	}
	var v Catalog
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer catalog: %w", err)
	}
	return &v, nil
}

// IsNotFound reports a 404 PeerError (old peer missing a v1 route).
func IsNotFound(err error) bool {
	var pe *PeerError
	return errors.As(err, &pe) && pe.Status == http.StatusNotFound
}

// VCall invokes one tool over POST /federation/v1/call (native envelope,
// verbatim blocks). 404 = old peer (caller falls back to thin Call).
func (c *Client) VCall(ctx context.Context, server, tool string, args map[string]any, meta map[string]any) (*mcp.CallToolResult, error) {
	body := map[string]any{"server": server, "tool": tool}
	if args != nil {
		body["arguments"] = args
	}
	if meta != nil {
		body["_meta"] = meta
	}
	req, err := c.req(ctx, http.MethodPost, "/federation/v1/call", body)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, tool)
	if err != nil {
		return nil, err
	}
	var v struct {
		Content           []map[string]any `json:"content"`
		IsError           bool             `json:"isError"`
		StructuredContent any              `json:"structuredContent"`
		Meta              map[string]any   `json:"_meta"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer vcall %q/%q: %w", server, tool, err)
	}
	res := &mcp.CallToolResult{IsError: v.IsError, StructuredContent: v.StructuredContent}
	if v.Meta != nil {
		res.Meta = mcp.NewMetaFromMap(v.Meta)
	}
	for _, cm := range v.Content {
		ct, err := mcp.ParseContent(cm)
		if err != nil {
			return nil, fmt.Errorf("peer vcall %q/%q: bad content: %w", server, tool, err)
		}
		res.Content = append(res.Content, ct)
	}
	return res, nil
}

// VResource reads one resource over POST /federation/v1/resource.
func (c *Client) VResource(ctx context.Context, server, uri string) (*mcp.ReadResourceResult, error) {
	req, err := c.req(ctx, http.MethodPost, "/federation/v1/resource",
		map[string]any{"server": server, "uri": uri})
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer vresource %q %q: %w", server, uri, err)
	}
	res := &mcp.ReadResourceResult{}
	for _, cm := range v.Contents {
		rc, err := mcp.ParseResourceContents(cm)
		if err != nil {
			return nil, fmt.Errorf("peer vresource %q %q: %w", server, uri, err)
		}
		res.Contents = append(res.Contents, rc)
	}
	return res, nil
}

// VPrompt renders one prompt over POST /federation/v1/prompt.
func (c *Client) VPrompt(ctx context.Context, server, name string, args map[string]string) (*mcp.GetPromptResult, error) {
	body := map[string]any{"server": server, "name": name}
	if args != nil {
		body["arguments"] = args
	}
	req, err := c.req(ctx, http.MethodPost, "/federation/v1/prompt", body)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    mcp.Role       `json:"role"`
			Content map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer vprompt %q/%q: %w", server, name, err)
	}
	res := &mcp.GetPromptResult{Description: v.Description}
	for _, m := range v.Messages {
		ct, err := mcp.ParseContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("peer vprompt %q/%q: bad message: %w", server, name, err)
		}
		res.Messages = append(res.Messages, mcp.PromptMessage{Role: m.Role, Content: ct})
	}
	return res, nil
}

// VComplete proxies one completion over POST /federation/v1/complete.
func (c *Client) VComplete(ctx context.Context, server string, params mcp.CompleteParams) (*mcp.CompleteResult, error) {
	req, err := c.req(ctx, http.MethodPost, "/federation/v1/complete", map[string]any{
		"server": server, "ref": params.Ref,
		"argument": params.Argument, "context": params.Context,
	})
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Completion mcp.Completion `json:"completion"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer vcomplete %q: %w", server, err)
	}
	return &mcp.CompleteResult{Completion: v.Completion}, nil
}

// Rev fetches the peer's catalog revision hash (cheap, never spawns there).
// 404 = old peer without /federation/v1 (caller skips tick-compare).
func (c *Client) Rev(ctx context.Context) (string, error) {
	req, err := c.req(ctx, http.MethodGet, "/federation/v1/rev", nil)
	if err != nil {
		return "", err
	}
	b, err := c.do(req, "", "")
	if err != nil {
		return "", err
	}
	var v struct {
		Rev string `json:"rev"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", fmt.Errorf("peer rev: %w", err)
	}
	return v.Rev, nil
}

// Health probes the peer daemon (existence + instance id + per-server health).
func (c *Client) Health(ctx context.Context) (*Health, error) {
	req, err := c.req(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, "", "")
	if err != nil {
		return nil, err
	}
	var h Health
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("peer /health: %w", err)
	}
	return &h, nil
}

// Tools lists one peer server's tools (thin: name+description).
func (c *Client) Tools(ctx context.Context, server string) ([]ToolStub, error) {
	req, err := c.req(ctx, http.MethodGet, "/servers/"+url.PathEscape(server)+"/tools", nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Tools []ToolStub `json:"tools"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer tools %q: %w", server, err)
	}
	return v.Tools, nil
}

// Describe fetches one peer tool's full schema.
func (c *Client) Describe(ctx context.Context, server, tool string) (*ToolSchema, error) {
	req, err := c.req(ctx, http.MethodGet,
		"/servers/"+url.PathEscape(server)+"/tools/"+url.PathEscape(tool), nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, tool)
	if err != nil {
		return nil, err
	}
	var s ToolSchema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("peer describe %q/%q: %w", server, tool, err)
	}
	return &s, nil
}

// ReadResource reads one peer resource body via ?uri=.
func (c *Client) ReadResource(ctx context.Context, server, uri string) ([]map[string]any, error) {
	q := url.Values{"uri": {uri}}.Encode()
	// {uri...} matches empty, so the trailing slash with only ?uri=
	// keeps the path tail empty (path+query together is a 400).
	req, err := c.req(ctx, http.MethodGet,
		"/servers/"+url.PathEscape(server)+"/resources/?"+q, nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer read %q %q: %w", server, uri, err)
	}
	return v.Contents, nil
}
func (c *Client) Call(ctx context.Context, server, tool string, parameters map[string]any) (*mcp.CallToolResult, error) {
	var body any
	if parameters != nil {
		body = map[string]any{"parameters": parameters}
	}
	req, err := c.req(ctx, http.MethodPost,
		"/servers/"+url.PathEscape(server)+"/tools/"+url.PathEscape(tool), body)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, tool)
	if err != nil {
		return nil, err
	}
	var v struct {
		Content           []map[string]any `json:"content"`
		IsError           bool             `json:"isError"`
		StructuredContent any              `json:"structuredContent"`
		Meta              map[string]any   `json:"_meta"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer call %q/%q: %w", server, tool, err)
	}
	res := &mcp.CallToolResult{IsError: v.IsError, StructuredContent: v.StructuredContent}
	if v.Meta != nil {
		res.Meta = mcp.NewMetaFromMap(v.Meta)
	}
	for _, cm := range v.Content {
		ct, err := mcp.ParseContent(cm)
		if err != nil {
			return nil, fmt.Errorf("peer call %q/%q: bad content: %w", server, tool, err)
		}
		res.Content = append(res.Content, ct)
	}
	return res, nil
}

// Resources lists one peer server's resources (+instructions).
func (c *Client) Resources(ctx context.Context, server string) ([]ResourceStub, string, error) {
	req, err := c.req(ctx, http.MethodGet, "/servers/"+url.PathEscape(server)+"/resources", nil)
	if err != nil {
		return nil, "", err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, "", err
	}
	var v struct {
		Resources    []ResourceStub `json:"resources"`
		Instructions string         `json:"instructions"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, "", fmt.Errorf("peer resources %q: %w", server, err)
	}
	return v.Resources, v.Instructions, nil
}

// Prompts lists one peer server's prompt templates.
func (c *Client) Prompts(ctx context.Context, server string) ([]mcp.Prompt, error) {
	req, err := c.req(ctx, http.MethodGet, "/servers/"+url.PathEscape(server)+"/prompts", nil)
	if err != nil {
		return nil, err
	}
	b, err := c.do(req, server, "")
	if err != nil {
		return nil, err
	}
	var v struct {
		Prompts []PromptStub `json:"prompts"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("peer prompts %q: %w", server, err)
	}
	out := make([]mcp.Prompt, 0, len(v.Prompts))
	for _, p := range v.Prompts {
		mp := mcp.Prompt{Name: p.Name, Description: p.Description}
		for _, a := range p.Arguments {
			mp.Arguments = append(mp.Arguments, mcp.PromptArgument{
				Name: a.Name, Description: a.Description, Required: a.Required,
			})
		}
		out = append(out, mp)
	}
	return out, nil
}
