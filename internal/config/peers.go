// Package config loads ~/.oxyp/oxyp.toml plus auto-discovered connectors.
//
// Reload semantics (user decision 2026-10-28): a bad entry NEVER breaks the
// daemon. Invalid servers are kept with ConfigError set so `oxyp status`
// reports them; everything else keeps running.
package config

import "strings"

// Peer mode constants: how a peer's servers join the local namespace.
const (
	// PeerModeMerge (default): peer servers appear as locals; bare names
	// resolve to the priority winner (local rank 0 beats peers by default).
	PeerModeMerge = "merge"
	// PeerModeNamespace: peer servers appear only qualified (peer:server);
	// no bare alias is claimed.
	PeerModeNamespace = "namespace"
)

// PeerEntry is one mounted oxyp daemon: [peers.<name>] in TOML.
// The key IS the name — no `name =` field inside an entry.
// Peers expand to N virtual servers; auth stays on the owner.
type PeerEntry struct {
	// Description is free text for humans (shows in peers list).
	// Example: "wsl box - holds linear token".
	Description string `toml:"description"`
	// URL is the peer daemon's base URL (http(s)://host:port).
	URL string `toml:"url"`
	// Token is the peer daemon's bearer token (empty = loopback trust).
	Token string `toml:"token"`
	// Mode is "merge" (default) or "namespace".
	Mode string `toml:"mode"`
	// Priority ranks this peer in merge conflicts; lower wins.
	// Local servers rank 0, so positive priorities lose to locals
	// and negative ones beat them.
	Priority int `toml:"priority"`
	// Timeout is the per-call budget in seconds for this peer's tools
	// (nil = daemon default 10s; 0 = default, not unlimited).
	Timeout *int `toml:"timeout"`
	// Include allowlists peer servers by advertised name (empty = all).
	// Entries match the full owner route (c:gamma) or the leaf (gamma).
	Include []string `toml:"include"`
	// Servers carries per-server overrides (priority only for now), keyed
	// by owner route (c:gamma) or leaf (gamma); full keys win.
	Servers map[string]PeerServer `toml:"servers"`
	// TLS is the peer connection's TLS config (same shape as servers).
	TLS *TLSConfig `toml:"tls"`
	// Enabled = false removes the peer entirely (nil = true).
	Enabled *bool `toml:"enabled"`

	// Name is the map key, set at load. ConfigError is set by validation,
	// never by users. A broken peer is skipped (its servers vanish);
	// the daemon keeps running.
	Name        string `toml:"-"`
	ConfigError string `toml:"-"`
}

// PeerServer carries per-server overrides within a peer.
type PeerServer struct {
	// Priority overrides the peer priority for one server (merge mode).
	Priority *int `toml:"priority"`
	// Timeout overrides the peer timeout for one server (seconds).
	Timeout *int `toml:"timeout"`
	// Tools is the importer's local tool policy for this peer server
	// (deny-means-invisible, same engine as locals). Owner gates re-run
	// downstream: both ends enforce, either can deny.
	Tools *ToolsPolicy `toml:"tools"`
	// Params restricts allowed arguments per tool, same shape as
	// [servers.x.params]. No entry = all parameters allowed.
	Params map[string][]string `toml:"params"`
}

// IsEnabled reports false only on explicit enabled=false.
func (p PeerEntry) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// ModeOr default-normalizes the mode ("" = merge).
func (p PeerEntry) ModeOr() string {
	if p.Mode == "" {
		return PeerModeMerge
	}
	return p.Mode
}

// ServerPriority resolves the merge rank for one peer server, named by
// owner route (c:gamma) or leaf: full-route override beats leaf override
// beats the peer default.
func (p PeerEntry) ServerPriority(server string) int {
	if s, ok := p.Servers[server]; ok && s.Priority != nil {
		return *s.Priority
	}
	if i := strings.LastIndexByte(server, ':'); i >= 0 {
		if s, ok := p.Servers[server[i+1:]]; ok && s.Priority != nil {
			return *s.Priority
		}
	}
	return p.Priority
}

// ServerPolicy resolves the importer's local tool policy for one peer
// server, named by owner route (c:gamma) or leaf: full-route beats leaf,
// nil = no local policy (owner gates only).
func (p PeerEntry) ServerPolicy(server string) *ToolsPolicy {
	if s, ok := p.Servers[server]; ok && s.Tools != nil {
		return s.Tools
	}
	if i := strings.LastIndexByte(server, ':'); i >= 0 {
		if s, ok := p.Servers[server[i+1:]]; ok && s.Tools != nil {
			return s.Tools
		}
	}
	return nil
}

// ServerParams resolves the importer's local param policy for one peer
// server (same precedence as ServerPolicy); nil = all allowed.
func (p PeerEntry) ServerParams(server string) map[string][]string {
	if s, ok := p.Servers[server]; ok && s.Params != nil {
		return s.Params
	}
	if i := strings.LastIndexByte(server, ':'); i >= 0 {
		if s, ok := p.Servers[server[i+1:]]; ok && s.Params != nil {
			return s.Params
		}
	}
	return nil
}

// ServerTimeout resolves the per-call budget for one peer server (seconds):
// per-server override beats the peer default; nil = daemon default.
func (p PeerEntry) ServerTimeout(server string) *int {
	if s, ok := p.Servers[server]; ok && s.Timeout != nil {
		return s.Timeout
	}
	if i := strings.LastIndexByte(server, ':'); i >= 0 {
		if s, ok := p.Servers[server[i+1:]]; ok && s.Timeout != nil {
			return s.Timeout
		}
	}
	return p.Timeout
}

// DeepCopy returns an independent copy (slices and sub-configs cloned).
func (p PeerEntry) DeepCopy() PeerEntry {
	out := p
	if p.Timeout != nil {
		t := *p.Timeout
		out.Timeout = &t
	}
	out.Include = append([]string(nil), p.Include...)
	if p.Servers != nil {
		out.Servers = make(map[string]PeerServer, len(p.Servers))
		for k, v := range p.Servers {
			if v.Priority != nil {
				pr := *v.Priority
				v.Priority = &pr
			}
			if v.Timeout != nil {
				t := *v.Timeout
				v.Timeout = &t
			}
			if v.Tools != nil {
				tp := *v.Tools
				tp.Allow = append([]string(nil), v.Tools.Allow...)
				tp.Deny = append([]string(nil), v.Tools.Deny...)
				v.Tools = &tp
			}
			if v.Params != nil {
				pm := make(map[string][]string, len(v.Params))
				for pk, pv := range v.Params {
					pm[pk] = append([]string(nil), pv...)
				}
				v.Params = pm
			}
			out.Servers[k] = v
		}
	}
	if p.TLS != nil {
		v := *p.TLS
		out.TLS = &v
	}
	if p.Enabled != nil {
		v := *p.Enabled
		out.Enabled = &v
	}
	return out
}

// Qualify builds the canonical virtual-server name for a peer server.
func Qualify(peer, server string) string {
	return peer + ":" + server
}
