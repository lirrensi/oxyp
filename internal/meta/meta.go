// Package meta exposes the oxyp meta-tools as plain HTTP handlers:
//
//	GET  /servers                    list_servers (?filter=)
//	GET  /servers/{name}/tools        list_tools (thin: name+description)
//	GET  /servers/{name}/tools/{tool} describe_tool (full schema)
//	POST /servers/{name}/tools/{tool} call ({parameters})
//	GET  /servers/{name}/resources    list downstream resources (+instructions)
//	GET  /servers/{name}/resources/{uri...} read one resource (?uri= also works)
//	GET  /servers/{name}/prompts      list downstream prompt templates
//	GET  /tools                       search_tools (?q=, ?server=, ?regex=1, ?limit=, ?offset=)
//	GET  /health                      daemon + per-server health
//	POST /reload                       re-read config (same as SIGHUP)
//
// Resource/prompt handlers are live passthrough: they spawn cold servers
// (no disk cache — resource bodies are live data, not stable metadata).
// Servers without resources/prompts support return an empty list, never
// an error, so callers can probe uniformly.
//
// C1 enforced everywhere: denied tools are invisible in list/describe and
// rejected at invoke, via the same perms.Resolve.
package meta

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/lirrensi/oxyp/internal/outguard"
	"github.com/lirrensi/oxyp/internal/perms"
	"github.com/lirrensi/oxyp/internal/redact"
	"github.com/mark3labs/mcp-go/mcp"
)

// Server wires HTTP handlers to a daemon + reload func.
type Server struct {
	d         *daemon.Daemon
	reload    func() (added, removed, modified []string, err error)
	token     string
	bindAddr  string
	pid       int
	startedAt string
	mux       *http.ServeMux
}

// New builds the handler set. token "" = no auth (loopback trust).
// bindAddr is the daemon listen address (host:port), used for Host checks.
func New(d *daemon.Daemon, reload func() (added, removed, modified []string, err error), token, bindAddr string) *Server {
	s := &Server{d: d, reload: reload, token: token, bindAddr: bindAddr, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /servers", s.handleServers)
	s.mux.HandleFunc("GET /servers/{name}/tools", s.handleListTools)
	s.mux.HandleFunc("GET /servers/{name}/tools/{tool}", s.handleDescribe)
	s.mux.HandleFunc("POST /servers/{name}/tools/{tool}", s.handleInvoke)
	s.mux.HandleFunc("GET /servers/{name}/resources", s.handleListResources)
	s.mux.HandleFunc("GET /servers/{name}/resources/{uri...}", s.handleReadResource)
	s.mux.HandleFunc("GET /servers/{name}/prompts", s.handleListPrompts)
	s.mux.HandleFunc("GET /tools", s.handleSearch)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /reload", s.handleReload)
	return s
}

// SetIdentity stamps the daemon's PID + start time onto /health so CLI
// probes can tell "our daemon" from a stranger on the same port.
func (s *Server) SetIdentity(pid int, startedAt string) {
	s.pid, s.startedAt = pid, startedAt
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			writeErr(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		if !checkHost(r, s.bindAddr) {
			writeErr(w, http.StatusForbidden, "foreign Host rejected")
			return
		}
		if !checkOrigin(r) {
			writeErr(w, http.StatusForbidden, "foreign Origin rejected")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Method != http.MethodOptions && r.ContentLength != 0 {
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeErr(w, http.StatusUnsupportedMediaType, "state-changing requests require application/json")
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

// checkHost validates the Host header. Loopback binds accept only
// loopback/localhost Hosts (DNS-rebinding defense); non-loopback binds
// accept any Host (token is the defense there).
func checkHost(r *http.Request, bindAddr string) bool {
	if !isLoopbackBind(bindAddr) {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackBind(bindAddr string) bool {
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return true // fail closed: treat as loopback
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || host == "localhost" {
		return host != ""
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkOrigin rejects a present-but-foreign Origin header. Absent Origin
// (CLI, curl, shim) passes. Loopback origins pass.
func checkOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	// Parse as URL first: Hostname() strips scheme, port, path, and
	// brackets correctly (SplitHostPort-first misparses IPv6 origins).
	host := ""
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		host = u.Hostname()
	} else {
		// Fallback for bare hosts: strip scheme/path manually.
		host = strings.ToLower(origin)
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type serverInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions,omitempty"`
	Healthy      *bool  `json:"healthy,omitempty"`
	Error        string `json:"error,omitempty"`
	AuthRequired string `json:"auth_required,omitempty"`
	Connected    bool   `json:"connected"`
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	filter := strings.ToLower(r.URL.Query().Get("filter"))
	var out []serverInfo
	for _, name := range s.d.Names() {
		e := s.d.Entry(name)
		if e == nil {
			continue // concurrent reload removed it
		}
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) &&
			!strings.Contains(strings.ToLower(e.Description), filter) {
			continue
		}
		info := serverInfo{Name: name, Description: e.Description}
		if e.ConfigError != "" {
			f := false
			info.Healthy = &f
			info.Error = "config: " + e.ConfigError
		} else if h := s.d.HealthOf(name); h != nil {
			info.Healthy = &h.Healthy
			info.Error = h.Error
			info.AuthRequired = h.AuthRequired
		}
		// Surface cached init-time instructions without spawning cold
		// servers: only already-connected servers have them. Never blocks.
		if s.d.IsConnected(name) {
			info.Connected = true
			if ins, ok := s.d.CachedInstructions(name); ok {
				info.Instructions = ins
			}
		}
		out = append(out, info)
	}
	if out == nil {
		out = []serverInfo{}
	}
	writeJSON(w, map[string]any{"servers": out})
}

type toolStub struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	e := s.d.Entry(name)
	if e == nil {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	tools, err := s.d.ListTools(r.Context(), name)
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	out := []toolStub{}
	for _, t := range tools {
		if perms.Resolve(e, t.Name).Action != "allow" {
			continue // C1: denied ⇒ invisible
		}
		out = append(out, toolStub{Name: t.Name, Description: t.Description})
	}
	writeJSON(w, map[string]any{"tools": out})
}

func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	name, tool := r.PathValue("name"), r.PathValue("tool")
	e := s.d.Entry(name)
	if e == nil {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	// C1: check visibility BEFORE touching downstream.
	if perms.Resolve(e, tool).Action != "allow" {
		writeErr(w, http.StatusNotFound, "tool not found")
		return
	}
	tools, err := s.d.ListTools(r.Context(), name)
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	for _, t := range tools {
		if strings.EqualFold(t.Name, tool) {
			writeJSON(w, map[string]any{
				"name": t.Name, "description": t.Description,
				"inputSchema": t.InputSchema,
			})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "tool not found")
}

// handleListResources lists downstream resources plus the server's
// init-time instructions (captured at connect, cached with tools).
// Servers without resources support return {"resources": []}.
func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.d.Entry(name) == nil {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	res, resErr := s.d.ListResources(r.Context(), name)
	// Instructions are init-time and independent of resources support:
	// fetch before the unsupported branch so both responses carry them.
	ins, _ := s.d.Instructions(r.Context(), name)
	if resErr != nil {
		if isUnsupported(resErr) {
			writeJSON(w, map[string]any{"resources": []any{}, "instructions": ins})
			return
		}
		writeDownstreamErr(w, resErr)
		return
	}
	out := []map[string]any{}
	for _, x := range res {
		out = append(out, map[string]any{
			"uri": x.URI, "name": x.Name,
			"description": x.Description, "mimeType": x.MIMEType,
		})
	}
	writeJSON(w, map[string]any{"resources": out, "instructions": ins})
}

// handleReadResource reads one downstream resource by URI. The URI comes
// from the path tail (/resources/{uri...}) or the ?uri= query param;
// exactly one must identify the resource.
func (s *Server) handleReadResource(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.d.Entry(name) == nil {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	uri := r.PathValue("uri")
	if q := r.URL.Query().Get("uri"); q != "" {
		if uri != "" {
			writeErr(w, http.StatusBadRequest, "use path or ?uri=, not both")
			return
		}
		uri = q
	}
	if uri == "" {
		writeErr(w, http.StatusBadRequest, "missing resource URI")
		return
	}
	res, err := s.d.ReadResource(r.Context(), name, uri)
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"contents": res.Contents})
}

// handleListPrompts lists downstream prompt templates. Servers without
// prompts support return {"prompts": []}.
func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.d.Entry(name) == nil {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	prompts, err := s.d.ListPrompts(r.Context(), name)
	if err != nil {
		if isUnsupported(err) {
			writeJSON(w, map[string]any{"prompts": []any{}})
			return
		}
		writeDownstreamErr(w, err)
		return
	}
	out := []map[string]any{}
	for _, p := range prompts {
		out = append(out, map[string]any{
			"name": p.Name, "description": p.Description,
			"arguments": p.Arguments,
		})
	}
	writeJSON(w, map[string]any{"prompts": out})
}

// isUnsupported reports a method-not-found from a server that does not
// implement resources/prompts. mcp-go surfaces it as ErrMethodNotFound;
// the string fallbacks cover wrapped variants (e.g. zoo's passthrough).
func isUnsupported(err error) bool {
	if errors.Is(err, mcp.ErrMethodNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "method not found") ||
		strings.Contains(msg, "not found: resources/") ||
		strings.Contains(msg, "not found: prompts/")
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hits, total, next, err := s.d.SearchTools(
		q.Get("server"), q.Get("q"), q.Get("regex") == "1" || q.Get("regex") == "true",
		atoiOr(q.Get("limit"), 12), atoiOr(q.Get("offset"), 0))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := []map[string]any{}
	for _, h := range hits {
		out = append(out, map[string]any{
			"server": h.Server, "name": h.Name,
			"description": h.Description, "score": h.Score,
		})
	}
	writeJSON(w, map[string]any{"tools": out, "total": total, "nextOffset": next})
}

func atoiOr(s string, dflt int) int {
	if s == "" {
		return dflt
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return dflt
	}
	return n
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	name, tool := r.PathValue("name"), r.PathValue("tool")
	e := s.d.Entry(name)
	if e == nil {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	dec := perms.Resolve(e, tool)
	if dec.Action != "allow" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "blocked by policy", "details": dec.Message,
			"_meta": map[string]string{"oxypPolicy": "blocked"},
		})
		return
	}
	var body struct {
		Parameters map[string]any `json:"parameters"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	res, err := s.d.CallTool(r.Context(), name, tool, body.Parameters)
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	content := res.Content
	if g := e.EffectiveGuard(s.d.DaemonConfig()); g != nil {
		content, _ = outguard.Apply(content, outguard.Limits{
			MaxBytes: g.MaxBytes, MaxLines: g.MaxLines,
		})
	}
	// Passthrough: content blocks verbatim (text/image/audio/resource),
	// structuredContent + _meta preserved. content[] stays authoritative —
	// some clients ignore it when structuredContent exists, so both ride.
	out := map[string]any{"content": content, "isError": res.IsError}
	if res.StructuredContent != nil {
		out["structuredContent"] = res.StructuredContent
	}
	if res.Meta != nil {
		out["_meta"] = res.Meta
	}
	writeJSON(w, out)
}

// writeDownstreamErr maps downstream failures: auth-needed gets 401 + the
// login hint (lazy-mcp AUTH_REQUIRED shape); everything else 502.
func writeDownstreamErr(w http.ResponseWriter, err error) {
	var authErr *daemon.AuthNeededError
	if errors.As(err, &authErr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":         redact.Body(err.Error()),
			"auth_required": "run: oxyp login " + authErr.Server,
		})
		return
	}
	writeErr(w, http.StatusBadGateway, redact.Body(err.Error()))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	servers := map[string]any{}
	for _, name := range s.d.Names() {
		e := s.d.Entry(name)
		if e == nil {
			continue // concurrent reload removed it
		}
		h := s.d.HealthOf(name)
		entry := map[string]any{"connected": s.d.IsConnected(name) && e.ConfigError == ""}
		if e.ConfigError != "" {
			entry["healthy"] = false
			entry["error"] = "config: " + e.ConfigError
		} else if h != nil {
			entry["healthy"] = h.Healthy
			if h.Error != "" {
				entry["error"] = redact.Body(h.Error)
			}
			if h.AuthRequired != "" {
				entry["auth_required"] = h.AuthRequired
			}
		}
		servers[name] = entry
	}
	writeJSON(w, map[string]any{"pid": s.pid, "startedAt": s.startedAt, "servers": servers})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	added, removed, modified, err := s.reload()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"added": added, "removed": removed, "modified": modified})
}
