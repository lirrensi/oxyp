package meta

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/lirrensi/oxyp/internal/outguard"
	peerfederation "github.com/lirrensi/oxyp/internal/peerfederation"
	"github.com/lirrensi/oxyp/internal/perms"
)

// Federation catalog contract, v1.
//
// GET /federation/v1/catalog answers one peer sync in a single round trip:
// every shared server with its full tools (name, description, inputSchema,
// title, annotations — the fields the thin CLI list drops), instructions,
// and health. Denied tools are omitted (owner gates apply before export).
// Shape is additive-safe: importers ignore unknown fields; absent tools
// (nil) means "unknown/unsupported", never an error.
//
// Local callers get the local view (Names); peers get pinned routes
// (PeerNames) through the sharing filter — same views as /servers.
func (s *Server) handleFederationCatalog(w http.ResponseWriter, r *http.Request) {
	peerView := peerfederation.IsPeer(r.Context())
	out, rev := s.catalogRows(r, peerView, true)
	writeJSON(w, map[string]any{
		"version":  "v1",
		"instance": s.d.InstanceID(),
		"name":     s.d.DaemonName(),
		"rev":      rev,
		"servers":  out,
	})
}

type toolFull struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Title        string `json:"title,omitempty"`
	InputSchema  any    `json:"inputSchema,omitempty"`
	OutputSchema any    `json:"outputSchema,omitempty"`
	Annotations  any    `json:"annotations,omitempty"`
	Icons        any    `json:"icons,omitempty"`
	Execution    any    `json:"execution,omitempty"`
	Meta         any    `json:"_meta,omitempty"`
}

type serverFull struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Origin       string                 `json:"origin,omitempty"`
	Instructions string                 `json:"instructions,omitempty"`
	Healthy      *bool                  `json:"healthy,omitempty"`
	Error        string                 `json:"error,omitempty"`
	AuthRequired string                 `json:"auth_required,omitempty"`
	State        string                 `json:"state,omitempty"`
	Connected    bool                   `json:"connected"`
	Tools        []toolFull             `json:"tools"`
	Transitive   bool                   `json:"transitive,omitempty"`
	Resources    []mcp.Resource         `json:"resources,omitempty"`
	Templates    []mcp.ResourceTemplate `json:"templates,omitempty"`
	Prompts      []mcp.Prompt           `json:"prompts,omitempty"`
}

func (s *Server) catalogRows(r *http.Request, peerView, full bool) ([]serverFull, string) {
	names := s.d.Names()
	if peerView {
		names = s.d.PeerNames()
	}
	out := []serverFull{}
	ctx := r.Context()
	for _, name := range names {
		if peerView && !s.d.SharedWithPeer(name) {
			continue
		}
		e := s.d.Entry(name)
		if e == nil {
			continue
		}
		sf := serverFull{Name: name, Description: e.Description, Tools: []toolFull{}}
		// Origin consent rides the row: locals read their flag; peer
		// virtuals carry what the owner advertised (stored at import).
		if e.Peer == "" {
			sf.Transitive = e.PeerExport != nil && e.PeerExport.Transitive
		} else {
			sf.Transitive = e.PeerTransitive
		}
		if e.Peer != "" {
			sf.Origin = e.Peer
		} else {
			sf.Origin = "local"
		}
		if h := s.d.HealthOf(name); h != nil {
			sf.Healthy = &h.Healthy
			sf.Error = h.Error
			sf.AuthRequired = h.AuthRequired
			sf.State = string(h.State)
		} else {
			sf.State = "cold"
		}
		if s.d.IsConnected(name) {
			sf.Connected = true
			if ins, ok := s.d.CachedInstructions(name); ok {
				sf.Instructions = ins
			}
		}
		if full {
			// Side-effect quarantine: best-effort fills below must not
			// mutate served health — a ListPrompts miss on a prompts-less
			// server would recordFailure, flip our rev, and drive importer
			// resync loops. Save/restore around the whole fill.
			savedHealth := s.d.HealthOf(name)
			fillDone := func() { s.d.RestoreHealth(name, savedHealth) }
			// Tools: memory/disk only when cold? No — ListTools may spawn,
			// exactly like thin list + describes would. One catalog = one
			// spawn at most per server, schemas included (no N+1).
			tools, err := s.d.ListTools(daemon.WithPassthrough(ctx, r.Header, e.PassthroughHeaders), name)
			if err != nil {
				// Unlistable server: row without tools (importer keeps
				// last-good). Never fail the whole catalog for one server.
				// Re-read the failure's health (the real signal) — the
				// pre-fill copy above is stale by construction.
				if h := s.d.HealthOf(name); h != nil {
					sf.Healthy = &h.Healthy
					sf.Error = h.Error
					sf.AuthRequired = h.AuthRequired
					sf.State = string(h.State)
				}
				out = append(out, sf)
				continue
			}
			for _, t := range tools {
				if perms.Resolve(e, t.Name).Action != "allow" {
					continue
				}
				tf := toolFull{Name: t.Name, Description: t.Description, Title: t.Title}
				tf.InputSchema = t.InputSchema
				if t.RawInputSchema != nil {
					tf.InputSchema = t.RawInputSchema
				}
				tf.OutputSchema = t.OutputSchema
				if t.RawOutputSchema != nil {
					tf.OutputSchema = t.RawOutputSchema
				}
				tf.Annotations = t.Annotations
				tf.Icons = t.Icons
				tf.Execution = t.Execution
				tf.Meta = t.Meta
				sf.Tools = append(sf.Tools, tf)
			}
			if res, rerr := s.d.ListResources(ctx, name); rerr == nil {
				sf.Resources = res
			}
			if tmpl, terr := s.d.ListResourceTemplates(ctx, name); terr == nil {
				sf.Templates = tmpl
			}
			if prompts, perr := s.d.ListPrompts(ctx, name); perr == nil {
				sf.Prompts = prompts
			}
			fillDone()
			// Re-read health AFTER any spawn above: the rev must hash the
			// served (post-spawn) state, not the pre-spawn read — otherwise
			// the catalog's own rev never matches the next /rev tick and
			// importers resync forever.
			if h := s.d.HealthOf(name); h != nil {
				sf.Healthy = &h.Healthy
				sf.Error = h.Error
				sf.AuthRequired = h.AuthRequired
			}
			if s.d.IsConnected(name) {
				sf.Connected = true
				if ins, ok := s.d.CachedInstructions(name); ok {
					sf.Instructions = ins
				}
			}
		} else {
			// Rev mode: names only, caches only, never spawns.
			// MUST mirror full-mode row shape exactly (same policy filter,
			// same res/template/prompt sets) or catalog.rev != /rev.rev and
			// importers resync forever (rev-mode rows feed the hash only).
			cached := s.d.CachedToolNames(name)
			sort.Strings(cached)
			for _, tn := range cached {
				if perms.Resolve(e, tn).Action != "allow" {
					continue
				}
				sf.Tools = append(sf.Tools, toolFull{Name: tn})
			}
			for _, u := range s.d.CachedResourceURIs(name) {
				sf.Resources = append(sf.Resources, mcp.Resource{URI: u})
			}
			for _, tn := range s.d.CachedTemplateNames(name) {
				sf.Templates = append(sf.Templates, mcp.ResourceTemplate{Name: tn})
			}
			for _, pn := range s.d.CachedPromptNames(name) {
				sf.Prompts = append(sf.Prompts, mcp.Prompt{Name: pn})
			}
		}
		out = append(out, sf)
	}
	return out, hashServerRows(out)
}

// hashServerRows folds rows into a stable 16-hex-char rev: names,
// descriptions, health markers, tool/resource/template/prompt names, and
// the effective Transitive consent (consent revocation must flip the rev so
// middles resync and re-evaluate re-export).
// Schemas and bodies are excluded (identity, not content).
func hashServerRows(rows []serverFull) string {
	h := fnv.New64a()
	// NOTE: rows arrive in sorted-name order (Names/PeerNames guarantee).
	for _, sf := range rows {
		_, _ = h.Write([]byte("\x00" + sf.Name + "\x00" + sf.Description))
		if sf.Transitive {
			_, _ = h.Write([]byte("\x00t"))
		}
		if sf.Healthy != nil {
			marker := "\x01"
			if !*sf.Healthy {
				marker = "\x01u"
			}
			_, _ = h.Write([]byte(marker + sf.Error + "\x00" + sf.AuthRequired))
		}
		// NOTE: names sorted before hashing — catalog rows carry live
		// order, rev rows carry cache order; the hash must be order-free
		// or the two never converge.
		tn := make([]string, 0, len(sf.Tools))
		for _, t := range sf.Tools {
			tn = append(tn, t.Name)
		}
		sort.Strings(tn)
		for _, n := range tn {
			_, _ = h.Write([]byte("\x02" + n))
		}
		rn := make([]string, 0, len(sf.Resources))
		for _, res := range sf.Resources {
			rn = append(rn, res.URI)
		}
		sort.Strings(rn)
		for _, n := range rn {
			_, _ = h.Write([]byte("\x03" + n))
		}
		yn := make([]string, 0, len(sf.Templates))
		for _, tmpl := range sf.Templates {
			yn = append(yn, tmpl.Name)
		}
		sort.Strings(yn)
		for _, n := range yn {
			_, _ = h.Write([]byte("\x04" + n))
		}
		pn := make([]string, 0, len(sf.Prompts))
		for _, pr := range sf.Prompts {
			pn = append(pn, pr.Name)
		}
		sort.Strings(pn)
		for _, n := range pn {
			_, _ = h.Write([]byte("\x05" + n))
		}
	}
	rev := make([]byte, 8)
	binary.BigEndian.PutUint64(rev, h.Sum64())
	return hex.EncodeToString(rev)
}

// Federation revision contract, v1.
//
// GET /federation/v1/rev answers the cheapest possible change question: a
// hash over the caller's catalog view (shared server names + per-server tool
// names + health markers). Importers poll it on the ping tick and resync on
// mismatch — push-equivalent freshness for list_changed without long-lived
// streams. Never spawns: tool data comes from memory/disk caches only
// (uncached servers contribute their name alone).
//
// Local callers get the local view; peers get pinned routes through the
// sharing filter — same views as /servers and /catalog.
func (s *Server) handleFederationRev(w http.ResponseWriter, r *http.Request) {
	peerView := peerfederation.IsPeer(r.Context())
	_, rev := s.catalogRows(r, peerView, false)
	writeJSON(w, map[string]any{"rev": rev})
}

// Federation call contract, v1.
//
// POST /federation/v1/call {server, tool, arguments?, meta?} invokes one tool
// and returns the native result envelope: {content[], isError,
// structuredContent?, _meta?} with content blocks verbatim (text/image/audio/
// resource_link/embedded — annotations + block _meta preserved). Owner gates
// (sharing, tools policy, params) run before the call; outguard applies to
// text (same as CLI invoke). Errors reuse the REST mapping (401 auth +
// auth_required, 403 policy/param, 502 downstream) so importers share one
// decoder.
func (s *Server) handleFederationCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server    string         `json:"server"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Meta      map[string]any `json:"_meta"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Server == "" || body.Tool == "" {
		writeErr(w, http.StatusBadRequest, "server and tool required")
		return
	}
	name := body.Server
	e := s.d.Entry(name)
	if e == nil || (peerfederation.IsPeer(r.Context()) && !s.d.SharedWithPeer(name)) {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	dec := perms.Resolve(e, body.Tool)
	if dec.Action != "allow" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "blocked by policy", "details": dec.Message,
			"_meta": map[string]string{"oxypPolicy": "blocked"},
		})
		return
	}
	res, err := s.d.CallToolWithMeta(daemon.WithPassthrough(r.Context(), r.Header, e.PassthroughHeaders), name, body.Tool, body.Arguments, body.Meta)
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
	out := map[string]any{"content": content, "isError": res.IsError}
	if res.StructuredContent != nil {
		out["structuredContent"] = res.StructuredContent
	}
	if res.Meta != nil {
		out["_meta"] = res.Meta
	}
	writeJSON(w, out)
}

// Federation resource contract, v1.
//
// POST /federation/v1/resource {server, uri} reads one resource and returns
// the native {contents[]} envelope (contents verbatim: text/blob, mimeType,
// _meta preserved). Sharing gate + traversal guard apply; unsupported maps
// to 502 with method-not-found so importers treat it as empty.
func (s *Server) handleFederationResource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server string `json:"server"`
		URI    string `json:"uri"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Server == "" || body.URI == "" {
		writeErr(w, http.StatusBadRequest, "server and uri required")
		return
	}
	if s.d.Entry(body.Server) == nil || (peerfederation.IsPeer(r.Context()) && !s.d.SharedWithPeer(body.Server)) {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	res, err := s.d.ReadResource(r.Context(), body.Server, body.URI)
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"contents": res.Contents})
}

// Federation prompt contract, v1.
//
// POST /federation/v1/prompt {server, name, arguments?} renders one prompt
// and returns the native {description?, messages[]} envelope (messages
// verbatim: role + content blocks). Sharing gate applies.
func (s *Server) handleFederationPrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server    string            `json:"server"`
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Server == "" || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "server and name required")
		return
	}
	if s.d.Entry(body.Server) == nil || (peerfederation.IsPeer(r.Context()) && !s.d.SharedWithPeer(body.Server)) {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	res, err := s.d.GetPrompt(r.Context(), body.Server, body.Name, body.Arguments)
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"description": res.Description, "messages": res.Messages})
}

// Federation completion contract, v1.
//
// POST /federation/v1/complete {server, ref, argument, context?} proxies one
// MCP completion request and returns the native {completion} envelope.
// Sharing gate applies.
func (s *Server) handleFederationComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server   string               `json:"server"`
		Ref      any                  `json:"ref"`
		Argument mcp.CompleteArgument `json:"argument"`
		Context  mcp.CompleteContext  `json:"context"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Server == "" {
		writeErr(w, http.StatusBadRequest, "server required")
		return
	}
	if s.d.Entry(body.Server) == nil || (peerfederation.IsPeer(r.Context()) && !s.d.SharedWithPeer(body.Server)) {
		writeErr(w, http.StatusNotFound, "server not found")
		return
	}
	res, err := s.d.Complete(r.Context(), body.Server, mcp.CompleteParams{
		Ref: body.Ref, Argument: body.Argument, Context: body.Context,
	})
	if err != nil {
		writeDownstreamErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"completion": res.Completion})
}
