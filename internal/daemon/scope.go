// Package daemon owns downstream MCP server processes.
package daemon

import (
	"net/url"
	"strings"

	"github.com/lirrensi/oxyp/internal/perms"
)

// Scope is the per-request client view: which servers this connection may
// see at all. It ANDs three dimensions:
//
//	Group (curated [groups.*] subset, "" = all) +
//	Allow (servers whitelist globs, empty = all) +
//	Deny  (exclude blacklist globs, deny always wins).
//
// Scope travels per-request — never daemon-global — so one client can see
// 5 servers while another sees all 10 on the same daemon:
//   - REST:   ?group= & ?servers= & ?exclude= (comma-separated, repeatable)
//   - shim:   --group/--servers/--exclude flags over OXYP_GROUP/OXYP_SERVERS/OXYP_EXCLUDE env
//   - /mcp:   tool args (group/servers/exclude) over ?query over env
//   - CLI:    --group/--servers/--exclude flags over env
//
// Matching is exact-ID-or-glob (* and ?, case-insensitive, same engine as
// tools allow/deny): "github", "wsl:*", "*-prod". Qualified names
// (peer:server, local:name) match on the user-facing name, its leaf, and
// its canonical key — so "gamma", "b:gamma", and "wsl:*" all do the sane
// thing whether you name the bare winner or the route.
type Scope struct {
	// Group is the curated subset name ("" = all).
	Group string
	// Allow is the servers whitelist (empty = all).
	Allow []string
	// Deny is the exclude blacklist (deny wins over allow).
	Deny []string
}

// IsEmpty reports whether the scope restricts nothing.
func (s Scope) IsEmpty() bool {
	return s.Group == "" && len(s.Allow) == 0 && len(s.Deny) == 0
}

// ParseScope builds a Scope from REST query values. Lists are
// comma-separated and repeatable: ?servers=a,b&servers=c == [a b c].
// Keys: group, servers, exclude.
func ParseScope(q url.Values) Scope {
	return Scope{
		Group: strings.TrimSpace(q.Get("group")),
		Allow: SplitListValues(q["servers"]),
		Deny:  SplitListValues(q["exclude"]),
	}
}

// SplitListValues flattens repeated query values, each comma-separated,
// into trimmed non-empty patterns. ["a,b", " c "] → ["a" "b" "c"].
func SplitListValues(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// ParseList parses one comma-separated flag/env string into patterns.
// "" → nil (allow-all). "a, b,,c" → [a b c].
func ParseList(s string) []string {
	return SplitListValues([]string{s})
}

// ParseArgList parses one gateway/shim tool arg into patterns. Accepts the
// wire shapes an agent might send: comma string ("a,b"), string slice,
// or mixed []any. Anything else → nil.
func ParseArgList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return ParseList(t)
	case []string:
		return SplitListValues(t)
	case []any:
		var raw []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			}
		}
		return SplitListValues(raw)
	default:
		return nil
	}
}

// InScope reports whether server is visible under scope. It ANDs group
// membership with the allow/exclude lists; deny wins. Unknown group fails
// closed (same as InGroup).
func (d *Daemon) InScope(scope Scope, server string) bool {
	if !d.InGroup(scope.Group, server) {
		return false
	}
	if len(scope.Deny) != 0 {
		if _, hit := perms.MatchAny(scope.Deny, server); hit {
			return false
		}
		if i := strings.LastIndexByte(server, ':'); i >= 0 {
			if _, hit := perms.MatchAny(scope.Deny, server[i+1:]); hit {
				return false
			}
		}
		if canon := d.ResolvedName(server); canon != server {
			if _, hit := perms.MatchAny(scope.Deny, canon); hit {
				return false
			}
		}
	}
	if len(scope.Allow) == 0 {
		return true
	}
	if _, hit := perms.MatchAny(scope.Allow, server); hit {
		return true
	}
	if i := strings.LastIndexByte(server, ':'); i >= 0 {
		if _, hit := perms.MatchAny(scope.Allow, server[i+1:]); hit {
			return true
		}
	}
	if canon := d.ResolvedName(server); canon != server {
		if _, hit := perms.MatchAny(scope.Allow, canon); hit {
			return true
		}
	}
	return false
}
