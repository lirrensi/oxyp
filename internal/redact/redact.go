// Package redact scrubs credentials from logs and error details.
// Stolen shape from lazy-mcp logger.ts: case-insensitive key match,
// recursive JSON-aware walk, byte-bounded truncation. Never touches wire data.
package redact

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Secret keys in NORMALIZED form (lowercase, no separators): matching
// strips separators so snake/camel/kebab spellings all hit.
var keys = map[string]bool{
	"authorization": true, "proxyauthorization": true,
	"token": true, "accesstoken": true, "refreshtoken": true,
	"authtoken": true, "apikey": true, "xapikey": true,
	"clientsecret": true,
	"password":     true, "secret": true,
}

// normKey lowercases and strips separators for spelling-insensitive match.
func normKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "_", "")
	k = strings.ReplaceAll(k, "-", "")
	k = strings.ReplaceAll(k, " ", "")
	return k
}

// isSecretKey reports whether k names a credential field.
func isSecretKey(k string) bool { return keys[normKey(k)] }

// Headers scrubs an http.Header-like map, returning a copy.
func Headers(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if isSecretKey(k) {
			out[k] = []string{"[REDACTED]"}
			continue
		}
		out[k] = v
	}
	return out
}

// HeaderMap scrubs a simple string map (e.g. configured headers for display).
func HeaderMap(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if isSecretKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = v
	}
	return out
}

// Body parses body as JSON when possible, redacts recursively, scrubs
// plain-text credential patterns, truncates to 8 KiB. Display-safe.
func Body(body string) string {
	const maxBytes = 8192
	var v any
	if json.Unmarshal([]byte(body), &v) == nil {
		v = walk(v, 0)
		if b, err := json.Marshal(v); err == nil {
			body = string(b)
		}
	} else {
		body = scrubText(body)
	}
	if len(body) > maxBytes {
		cut := body[:maxBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		return cut + "…[truncated]"
	}
	return body
}

// scrubText masks plain-text credential patterns: `Bearer <tok>`,
// `Basic <tok>`, and `key=value` / `key: value` for secret keys.
// Whitespace-preserving (regex on the raw string).
var (
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)(\S+)`)
	// Optional quotes around key and value catch embedded JSON
	// (`"access_token":"SECRET"`) as well as bare pairs.
	kvRe = regexp.MustCompile(`(?i)"?([A-Za-z][A-Za-z0-9_.-]*)"?\s*([=:])\s*"?([^\s,;}\]]+)`)
)

func scrubText(s string) string {
	s = bearerRe.ReplaceAllString(s, "${1}${2}[REDACTED]")
	return kvRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := kvRe.FindStringSubmatch(m)
		if len(parts) != 4 || !isSecretKey(parts[1]) {
			return m
		}
		if lv := strings.ToLower(strings.Trim(parts[3], `"`)); lv == "bearer" || lv == "basic" {
			return m // bearerRe owns `Key: Bearer <tok>` shapes
		}
		// Preserve key + separator shape, mask the value (keep a
		// trailing quote when the value was quoted).
		sep := strings.IndexAny(m, "=:")
		pre := m[:sep+1]
		rest := m[sep+1:]
		if strings.HasPrefix(rest, " ") {
			pre += " "
		}
		q := ""
		if strings.HasSuffix(strings.TrimRight(m, " "), `"`) {
			q = `"`
		}
		return pre + "[REDACTED]" + q
	})
}

func walk(v any, depth int) any {
	if depth > 32 {
		return "[DEEP]"
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSecretKey(k) {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = walk(val, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = walk(val, depth+1)
		}
		return out
	default:
		return v
	}
}

// URL strips query/fragment/userinfo from rawurl: scheme://host/path.
// Unparseable input (or missing host) returns "[invalid-url]".
func URL(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil || u.Host == "" {
		return "[invalid-url]"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
