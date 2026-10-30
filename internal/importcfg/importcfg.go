// Package importcfg translates third-party MCP client configs into oxyp TOML.
//
// Supported sources (candidate files resolved from $HOME, first hit wins):
//
//	cursor	~/.cursor/mcp.json, ~/.config/Cursor/mcp.json	{"mcpServers": ...}
//	claude	~/.claude.json (+ Claude Desktop macOS path)	{"mcpServers": ...}
//	vscode	Code User settings.json / mcp.json (linux + mac)	{"mcp": {"servers": ...}}
//	opencode	~/.config/opencode/opencode.jsonc	{"mcp": ...} (JSONC)
//	omp	~/.omp/agent/mcp.json	{"mcpServers": ...}
package importcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lirrensi/oxyp/internal/config"
)

// Source is one third-party app and its candidate config files (first hit wins).
type Source struct {
	Name  string
	Files []string
}

// Sources returns the known app configs resolved against home.
func Sources(home string) []Source {
	macUser := filepath.Join(home, "Library", "Application Support")
	return []Source{
		{Name: "cursor", Files: []string{
			filepath.Join(home, ".cursor", "mcp.json"),
			filepath.Join(home, ".config", "Cursor", "mcp.json"),
		}},
		{Name: "claude", Files: []string{
			filepath.Join(home, ".claude.json"),
			filepath.Join(macUser, "Claude", "claude_desktop_config.json"),
		}},
		{Name: "vscode", Files: []string{
			filepath.Join(home, ".config", "Code", "User", "settings.json"),
			filepath.Join(macUser, "Code", "User", "settings.json"),
			filepath.Join(home, ".config", "Code", "User", "mcp.json"),
			filepath.Join(macUser, "Code", "User", "mcp.json"),
		}},
		{Name: "opencode", Files: []string{
			filepath.Join(home, ".config", "opencode", "opencode.jsonc"),
			filepath.Join(home, ".config", "opencode", "opencode.json"),
		}},
		{Name: "omp", Files: []string{
			filepath.Join(home, ".omp", "agent", "mcp.json"),
		}},
	}
}

// FindFile returns the first candidate that exists, or "".
func FindFile(s Source) string {
	for _, f := range s.Files {
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			return f
		}
	}
	return ""
}

// RawEntry is one third-party server entry in normalized form.
type RawEntry struct {
	Name      string
	Command   []string
	URL       string
	Env       map[string]string
	Headers   map[string]string
	Transport string // "sse" or "" (auto)
	OAuth     bool
	Enabled   *bool
	Cwd       string
	Dropped   []string // human notes for fields oxyp has no equivalent for
}

// flexStrList decodes a string or an array of strings (omp uses a bare
// string command, opencode uses an array).
type flexStrList []string

func (f *flexStrList) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = []string{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	*f = l
	return nil
}

// genericEntry covers the cursor/claude/vscode-classic/opencode/omp shapes.
type genericEntry struct {
	Command     flexStrList       `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	Environment map[string]string `json:"environment"` // opencode local
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Type        string            `json:"type"`
	Cwd         string            `json:"cwd"`
	Enabled     *bool             `json:"enabled"`
	OAuth       any               `json:"oauth"`
	Auth        any               `json:"auth"`
	Timeout     any               `json:"timeout"`
}

func oauthOn(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s != "" && s != "false" && s != "none" && s != "disabled"
	case map[string]any:
		return true // key present (even {}) = oauth configured
	default:
		return true
	}
}

func normalize(name string, g genericEntry) RawEntry {
	r := RawEntry{Name: name, Enabled: g.Enabled}
	r.Command = append([]string(g.Command), g.Args...)
	env := map[string]string{}
	for k, v := range g.Env {
		env[k] = v
	}
	for k, v := range g.Environment {
		env[k] = v
	}
	if len(env) > 0 {
		r.Env = env
	}
	r.URL = strings.TrimSpace(g.URL)
	if len(g.Headers) > 0 {
		r.Headers = g.Headers
	}
	if strings.EqualFold(strings.TrimSpace(g.Type), "sse") {
		r.Transport = "sse"
	}
	if oauthOn(g.OAuth) || oauthOn(g.Auth) {
		r.OAuth = true
	}
	if g.Cwd != "" {
		r.Cwd = g.Cwd
	}
	if g.Timeout != nil {
		r.Dropped = append(r.Dropped, "timeout dropped (oxyp uses daemon timeouts)")
	}
	return r
}

// ParseMCPServerMap parses {"mcpServers": {name: entry}} (cursor, claude, omp,
// bare vscode mcp.json).
func ParseMCPServerMap(data []byte) (map[string]RawEntry, error) {
	var top struct {
		Servers map[string]genericEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	if top.Servers == nil {
		return nil, fmt.Errorf("no mcpServers key")
	}
	out := make(map[string]RawEntry, len(top.Servers))
	for n, g := range top.Servers {
		out[n] = normalize(n, g)
	}
	return out, nil
}

// ParseVSCode parses VS Code settings.json ({"mcp": {"servers": ...}},
// tolerating a top-level "mcpServers" too), a dedicated mcp.json with a
// top-level "servers" object, or a bare mcp.json.
func ParseVSCode(data []byte) (map[string]RawEntry, error) {
	var settings struct {
		MCP struct {
			Servers map[string]genericEntry `json:"servers"`
		} `json:"mcp"`
		Servers    map[string]genericEntry `json:"mcpServers"`
		TopServers map[string]genericEntry `json:"servers"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	merged := map[string]genericEntry{}
	for n, g := range settings.MCP.Servers {
		merged[n] = g
	}
	for n, g := range settings.TopServers {
		merged[n] = g
	}
	for n, g := range settings.Servers {
		merged[n] = g
	}
	if len(merged) == 0 {
		return ParseMCPServerMap(data)
	}
	out := make(map[string]RawEntry, len(merged))
	for n, g := range merged {
		out[n] = normalize(n, g)
	}
	return out, nil
}

// ParseOpenCode parses opencode.jsonc (JSONC comments stripped first).
func ParseOpenCode(data []byte) (map[string]RawEntry, error) {
	var top struct {
		MCP map[string]genericEntry `json:"mcp"`
	}
	if err := json.Unmarshal(StripJSONC(data), &top); err != nil {
		return nil, err
	}
	if top.MCP == nil {
		return nil, fmt.Errorf("no mcp key")
	}
	out := make(map[string]RawEntry, len(top.MCP))
	for n, g := range top.MCP {
		out[n] = normalize(n, g)
	}
	return out, nil
}

// Parse dispatches on app name (cursor|claude|vscode|opencode|omp).
func Parse(app string, data []byte) (map[string]RawEntry, error) {
	switch app {
	case "cursor", "claude", "omp":
		return ParseMCPServerMap(data)
	case "vscode":
		return ParseVSCode(data)
	case "opencode":
		return ParseOpenCode(data)
	default:
		return nil, fmt.Errorf("unknown source %q (want cursor|claude|vscode|opencode|omp)", app)
	}
}

// StripJSONC removes // and /* */ comments, respecting double-quoted strings
// (so "https://..." URLs survive).
func StripJSONC(data []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(data))
	inStr := false
	esc := false
	i := 0
	for i < len(data) {
		c := data[i]
		if inStr {
			out.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			i++
			continue
		}
		if c == '"' {
			inStr = true
			out.WriteByte(c)
			i++
			continue
		}
		if c == '/' && i+1 < len(data) {
			switch data[i+1] {
			case '/':
				for i < len(data) && data[i] != '\n' {
					i++
				}
				continue
			case '*':
				i += 2
				for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
					i++
				}
				i += 2
				continue
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.Bytes()
}

// ToOxyp converts a normalized entry to an oxyp ServerEntry plus render notes.
// Notes describe dropped fields and oauth handling; they are emitted as TOML
// comments so the preview is exactly what gets written.
func ToOxyp(app string, r RawEntry) (config.ServerEntry, []string, error) {
	notes := append([]string{}, r.Dropped...)
	e := config.ServerEntry{Description: "Imported from " + app}
	if r.Enabled != nil {
		e.Enabled = r.Enabled
	}
	switch {
	case r.URL != "":
		e.URL = r.URL
		e.Headers = r.Headers
		e.Transport = r.Transport
		if r.OAuth {
			e.Auth = "oauth"
			notes = append(notes, `upstream oauth detected: auth = "oauth" set, verify with: oxyp login <name>`)
		}
		if len(r.Command) > 0 {
			notes = append(notes, "entry had both command and url; kept url")
		}
	case len(r.Command) > 0:
		e.Command = r.Command
		e.Env = r.Env
		e.Cwd = r.Cwd
	default:
		return e, notes, fmt.Errorf("entry %q has neither command nor url", r.Name)
	}
	return e, notes, nil
}

// RenderBlock renders the exact TOML text for one [servers.<name>] entry.
// notes become "# note: ..." comment lines above the table.
// Emission is manual (never a bare [servers] super-header): blocks
// concatenate safely and append to files that already have servers.
// Maps render as inline tables so no sub-table headers are emitted.
func RenderBlock(name string, e config.ServerEntry, notes []string) (string, error) {
	var buf bytes.Buffer
	for _, n := range notes {
		// Notes come from foreign configs: flatten newlines so a note
		// can never inject TOML tables into oxyp.toml.
		n = strings.ReplaceAll(strings.ReplaceAll(n, "\n", " "), "\r", " ")
		fmt.Fprintf(&buf, "# note: %s\n", n)
	}
	fmt.Fprintf(&buf, "[servers.%s]\n", tomlKey(name))
	if e.Description != "" {
		fmt.Fprintf(&buf, "description = %q\n", e.Description)
	}
	if len(e.Command) > 0 {
		fmt.Fprintf(&buf, "command = [")
		for i, a := range e.Command {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, "%q", a)
		}
		buf.WriteString("]\n")
	}
	if e.URL != "" {
		fmt.Fprintf(&buf, "url = %q\n", e.URL)
	}
	if e.Transport != "" {
		fmt.Fprintf(&buf, "transport = %q\n", e.Transport)
	}
	writeInlineMap(&buf, "headers", e.Headers)
	writeInlineMap(&buf, "env", e.Env)
	if e.Cwd != "" {
		fmt.Fprintf(&buf, "cwd = %q\n", e.Cwd)
	}
	if e.Enabled != nil {
		fmt.Fprintf(&buf, "enabled = %v\n", *e.Enabled)
	}
	if e.Auth != "" {
		fmt.Fprintf(&buf, "auth = %q\n", e.Auth)
	}
	return buf.String(), nil
}

// writeInlineMap emits `key = { K = "V", ... }` (sorted keys) or nothing
// when empty. Inline tables keep the block to a single [servers.<name>]
// header with no sub-tables.
func writeInlineMap(buf *bytes.Buffer, key string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	fmt.Fprintf(buf, "%s = { ", key)
	for i, k := range ks {
		if i > 0 {
			buf.WriteString(", ")
		}
		fmt.Fprintf(buf, "%q = %q", k, m[k])
	}
	buf.WriteString(" }\n")
}

// tomlKey quotes a table key unless it is a bare key.
func tomlKey(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}

// NamedBlock is one rendered server block in output order.
type NamedBlock struct {
	Name string
	Text string
}

// RenderAll joins blocks with a blank line; the result ends with "\n".
func RenderAll(blocks []NamedBlock) string {
	var sb strings.Builder
	for i, b := range blocks {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(b.Text)
	}
	out := sb.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// ConfigPath returns dir/oxyp.toml.
func ConfigPath(dir string) string { return filepath.Join(dir, "oxyp.toml") }

// AppendBlocks appends TOML text to dir/oxyp.toml, creating the file (and dir)
// if missing. New files are 0600; existing group/world-readable files are
// tightened to 0600. Callers print the same text as the preview beforehand.
// It returns a non-empty note when an existing file was tightened.
func AppendBlocks(dir, text string) (string, error) {
	path := ConfigPath(dir)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			return "", err
		}
		return "", nil
	}
	note := ""
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err == nil {
			note = fmt.Sprintf("note: tightened %s to 0600 (held credentials)", path)
		}
	}
	sep := "\n"
	if len(b) > 0 && b[len(b)-1] != '\n' {
		sep = "\n\n"
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(sep + text); err != nil {
		return "", err
	}
	return note, nil
}

// SanitizeName lowercases and maps runs of non [a-z0-9] to "-".
func SanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var sb strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && sb.Len() > 0 {
				sb.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "server"
	}
	return out
}

// NameFromURL derives a server name from a remote URL's hostname
// (dots become dashes, e.g. https://company.glean.com/mcp -> company-glean-com).
func NameFromURL(rawurl string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad url %q", rawurl)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url must start with http(s)://: %q", rawurl)
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	return SanitizeName(strings.ReplaceAll(host, ".", "-")), nil
}

// UniqueName sanitizes base then returns it if free, else base-2, base-3,
// ... (case-insensitive).
func UniqueName(base string, taken map[string]bool) string {
	base = SanitizeName(base)
	lower := make(map[string]bool, len(taken))
	for k := range taken {
		lower[strings.ToLower(k)] = true
	}
	if !lower[base] {
		return base
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d", base, i)
		if !lower[c] {
			return c
		}
	}
}
