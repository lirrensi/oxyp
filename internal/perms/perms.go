// Package perms implements per-server tool filtering (C1).
//
// Port of lazy-mcp's glob engine (* and ?, case-insensitive, ** collapsed
// as a ReDoS defuse). Semantics: deny always wins; nil policy or empty
// allow = allow all. Deny ⇒ invisible: callers filter discovery
// (list/describe) AND block invoke with the same decision function.
package perms

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/lirrensi/oxyp/internal/config"
)

// Decision is the outcome for one (server, tool) pair.
type Decision struct {
	Action  string // allow|deny
	Message string
}

// globToRegexp converts * → .*, ? → ., everything else literal.
// Collapses ** runs first (ReDoS defuse, same as lazy-mcp).
func globToRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?i)^")
	stars := false
	for _, r := range pattern {
		switch r {
		case '*':
			if !stars {
				b.WriteString(".*")
				stars = true
			}
		case '?':
			stars = false
			b.WriteString(".")
		default:
			stars = false
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func matches(pattern, value string) bool {
	return globToRegexp(pattern).MatchString(value)
}

func matchAny(patterns []string, tool string) (string, bool) {
	for _, p := range patterns {
		if matches(p, tool) {
			return p, true
		}
	}
	return "", false
}

// Resolve evaluates a server's tool policy: deny wins, then allow.
// Missing policy or empty allow = allow all.
func Resolve(srv *config.ServerEntry, tool string) Decision {
	if srv == nil || srv.Tools == nil {
		return Decision{Action: "allow"}
	}
	if p, ok := matchAny(srv.Tools.Deny, tool); ok {
		return Decision{Action: "deny",
			Message: fmt.Sprintf("matched deny pattern %q on server %q", p, srv.Name)}
	}
	if len(srv.Tools.Allow) == 0 {
		return Decision{Action: "allow"}
	}
	if _, ok := matchAny(srv.Tools.Allow, tool); ok {
		return Decision{Action: "allow"}
	}
	return Decision{Action: "deny",
		Message: fmt.Sprintf("not matched by any allow pattern on server %q", srv.Name)}
}

// Visible filters tool names to those allowed (C1: denied ⇒ invisible).
func Visible(srv *config.ServerEntry, tools []string) []string {
	out := tools[:0:0]
	for _, t := range tools {
		if Resolve(srv, t).Action == "allow" {
			out = append(out, t)
		}
	}
	return out
}
