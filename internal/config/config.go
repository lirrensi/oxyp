// Package config loads ~/.oxyp/oxyp.toml plus auto-discovered connectors.
//
// Reload semantics (user decision 2026-10-28): a bad entry NEVER breaks the
// daemon. Invalid servers are kept with ConfigError set so `oxyp status`
// reports them; everything else keeps running.
package config

import (
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultDir resolves the oxyp dir: OXYP_DIR > XDG_CONFIG_HOME/oxyp >
// ~/.oxyp. The --dir flag beats all of these (handled by the CLI).
func DefaultDir() string {
	if d := os.Getenv("OXYP_DIR"); d != "" {
		return d
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "oxyp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".oxyp"
	}
	return filepath.Join(home, ".oxyp")
}

// Config is the top-level oxyp.toml shape.
// Servers is keyed by name: [servers.<name>] in TOML. The key IS the name —
// no `name =` field inside an entry.
type Config struct {
	Daemon  DaemonConfig           `toml:"daemon"`
	Servers map[string]ServerEntry `toml:"servers"`
}

// DaemonConfig holds daemon-wide settings.
type DaemonConfig struct {
	// Addr is host:port for the HTTP API. Default 127.0.0.1:8379.
	Addr string `toml:"addr"`
	// Token is the bearer token for HTTP clients. Empty = loopback trust.
	Token string `toml:"token"`
	// ConnectorsDir lets tests override; default <dir>/connectors.
	ConnectorsDir string `toml:"connectors_dir"`
	// IdleTimeout is minutes before an idle stdio child is stopped.
	// Default 10. 0 = never. Per-server idle_timeout overrides.
	IdleTimeout *int `toml:"idle_timeout"`
	// OutputGuard caps inline text results (spill to 0600 file). Nil = off.
	OutputGuard *OutputGuardConfig `toml:"output_guard"`
	// Trace enables metadata-only JSONL tracing. Nil/false = off.
	Trace *TraceConfig `toml:"trace"`
}

// DeepCopy clones pointer fields so daemon readers never alias live config.
func (d DaemonConfig) DeepCopy() DaemonConfig {
	out := d
	if d.IdleTimeout != nil {
		v := *d.IdleTimeout
		out.IdleTimeout = &v
	}
	if d.OutputGuard != nil {
		v := *d.OutputGuard
		if d.OutputGuard.Enabled != nil {
			e := *d.OutputGuard.Enabled
			v.Enabled = &e
		}
		out.OutputGuard = &v
	}
	if d.Trace != nil {
		v := *d.Trace
		out.Trace = &v
	}
	return out
}

// OutputGuardConfig caps text result size. Media blocks are NEVER touched.
// When armed and text exceeds MaxBytes/MaxLines, inline text is cut to a
// head preview and the full text spills to a 0600 temp file whose path is
// included in the result. Enabled=false disables the guard for that scope
// (per-server opt-out); nil guard = inherit/off.
type OutputGuardConfig struct {
	Enabled  *bool `toml:"enabled"`   // nil = inherit (server) / armed (daemon); false = off
	MaxBytes int   `toml:"max_bytes"` // default 51200
	MaxLines int   `toml:"max_lines"` // default 2000
}

// TraceConfig enables metadata-only protocol tracing.
type TraceConfig struct {
	Enabled   bool   `toml:"enabled"`
	File      string `toml:"file"`       // default <dir>/trace.jsonl
	MaxBytes  int    `toml:"max_bytes"`  // default 262144, rotate cap
	MaxEvents int    `toml:"max_events"` // default 10000, ring cap
}

// ServerEntry is one downstream MCP server: local (command) or remote (url).
// Lives under [servers.<name>]; Name is filled from the map key at load and
// never read from TOML.
type ServerEntry struct {
	Description string            `toml:"description"`
	Command     []string          `toml:"command"`   // local: argv; connectors autodiscovery fills this
	URL         string            `toml:"url"`       // remote: Streamable HTTP endpoint
	Transport   string            `toml:"transport"` // remote: "" (auto) or "sse"
	Headers     map[string]string `toml:"headers"`   // remote: static headers
	Env         map[string]string `toml:"env"`       // local: child env additions
	// Cwd is the child workdir for local servers ("" = daemon's cwd).
	Cwd string `toml:"cwd"`
	// InheritEnv controls whether the child inherits the daemon's env.
	// Default true. False = clean minimal env (PATH+HOME+SYSTEMROOT only)
	// plus explicit env overlays. Local-only.
	InheritEnv *bool        `toml:"inherit_env"`
	Enabled    *bool        `toml:"enabled"` // nil = true
	Tools      *ToolsPolicy `toml:"tools"`   // per-server tool filter; nil = allow all
	TLS        *TLSConfig   `toml:"tls"`     // remote: per-server TLS
	Auth       string       `toml:"auth"`    // remote: "" or "oauth"
	OAuth      *OAuthConfig `toml:"oauth"`   // remote: client overrides
	// Lifecycle: "" (lazy) | eager | keep-alive | lazy-keep-alive.
	Lifecycle string `toml:"lifecycle"`
	// IdleTimeout overrides daemon idle_timeout for this server (minutes).
	IdleTimeout *int `toml:"idle_timeout"`
	// OutputGuard overrides daemon output_guard ({ enabled = false } disables).
	OutputGuard *OutputGuardConfig `toml:"output_guard"`
	// Trace overrides daemon trace for this server.
	Trace *TraceConfig `toml:"trace"`

	// Name is the map key, set at load. ConfigError is set by validation,
	// never by users. Server stays listed but unhealthy; daemon keeps running.
	Name        string `toml:"-"`
	ConfigError string `toml:"-"`
	// FromConnector is the discovered file path, for status reporting.
	FromConnector string `toml:"-"`
}

// DeepCopy returns an independent copy (maps, slices, and sub-configs
// cloned). Daemon.Entry hands these out so callers can read lock-free.
func (s ServerEntry) DeepCopy() ServerEntry {
	out := s
	out.Command = slices.Clone(s.Command)
	out.Headers = maps.Clone(s.Headers)
	out.Env = maps.Clone(s.Env)
	if s.InheritEnv != nil {
		v := *s.InheritEnv
		out.InheritEnv = &v
	}
	if s.Enabled != nil {
		v := *s.Enabled
		out.Enabled = &v
	}
	if s.Tools != nil {
		t := *s.Tools
		t.Allow = slices.Clone(s.Tools.Allow)
		t.Deny = slices.Clone(s.Tools.Deny)
		out.Tools = &t
	}
	if s.TLS != nil {
		v := *s.TLS
		out.TLS = &v
	}
	if s.OAuth != nil {
		v := *s.OAuth
		out.OAuth = &v
	}
	if s.IdleTimeout != nil {
		v := *s.IdleTimeout
		out.IdleTimeout = &v
	}
	if s.OutputGuard != nil {
		v := *s.OutputGuard
		if s.OutputGuard.Enabled != nil {
			e := *s.OutputGuard.Enabled
			v.Enabled = &e
		}
		out.OutputGuard = &v
	}
	if s.Trace != nil {
		v := *s.Trace
		out.Trace = &v
	}
	return out
}

// IsRemote reports whether this entry is a remote (URL) server.
func (s ServerEntry) IsRemote() bool { return s.URL != "" }

// IsEnabled reports false only on explicit enabled=false.
func (s ServerEntry) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// DefaultIdleTimeoutMin is the idle-stop default (minutes) when neither
// server nor daemon sets one.
const DefaultIdleTimeoutMin = 10

// EffectiveIdleTimeout resolves server > daemon > default (minutes).
// 0 = never stop.
func (s ServerEntry) EffectiveIdleTimeout(d DaemonConfig) int {
	if s.IdleTimeout != nil {
		return *s.IdleTimeout
	}
	if d.IdleTimeout != nil {
		return *d.IdleTimeout
	}
	return DefaultIdleTimeoutMin
}

// LifecycleMode normalizes "" to "lazy".
func (s ServerEntry) LifecycleMode() string {
	if s.Lifecycle == "" {
		return "lazy"
	}
	return s.Lifecycle
}

// WantsIdleStop reports whether the sweeper may stop this server.
// keep-alive modes and 0 timeouts are exempt.
func (s ServerEntry) WantsIdleStop(d DaemonConfig) bool {
	switch s.LifecycleMode() {
	case "keep-alive", "lazy-keep-alive":
		return false
	}
	if s.LifecycleMode() == "eager" && s.IdleTimeout == nil && d.IdleTimeout == nil {
		return false // eager: no idle stop unless explicitly configured
	}
	return s.EffectiveIdleTimeout(d) > 0
}

// WantsEagerStart reports whether the daemon connects at startup.
func (s ServerEntry) WantsEagerStart() bool {
	return s.LifecycleMode() == "eager" || s.LifecycleMode() == "keep-alive"
}

// Default output guard limits (adapter parity: 50 KiB / 2000 lines).
const (
	DefaultGuardMaxBytes = 51200
	DefaultGuardMaxLines = 2000
)

// EffectiveGuard resolves server > daemon guard config. Nil = off.
// A server-level Enabled=false disables the daemon-wide guard for that
// server (no inherit). Daemon-level Enabled=false is off.
func (s ServerEntry) EffectiveGuard(d DaemonConfig) *OutputGuardConfig {
	if s.OutputGuard != nil {
		if s.OutputGuard.Enabled != nil && !*s.OutputGuard.Enabled {
			return nil
		}
		return s.OutputGuard.withDefaults()
	}
	if d.OutputGuard != nil {
		if d.OutputGuard.Enabled != nil && !*d.OutputGuard.Enabled {
			return nil
		}
		return d.OutputGuard.withDefaults()
	}
	return nil
}

func (g *OutputGuardConfig) withDefaults() *OutputGuardConfig {
	out := *g
	if out.MaxBytes == 0 {
		out.MaxBytes = DefaultGuardMaxBytes
	}
	if out.MaxLines == 0 {
		out.MaxLines = DefaultGuardMaxLines
	}
	return &out
}

// Default trace bounds.
const (
	DefaultTraceMaxBytes  = 262144
	DefaultTraceMaxEvents = 10000
)

// EffectiveTrace resolves server > daemon trace config. Nil = off
// (Enabled=false also off).
func (s ServerEntry) EffectiveTrace(d DaemonConfig) *TraceConfig {
	if s.Trace != nil {
		return s.Trace.withDefaults()
	}
	if d.Trace != nil {
		return d.Trace.withDefaults()
	}
	return nil
}

func (t *TraceConfig) withDefaults() *TraceConfig {
	out := *t
	if out.MaxBytes == 0 {
		out.MaxBytes = DefaultTraceMaxBytes
	}
	if out.MaxEvents == 0 {
		out.MaxEvents = DefaultTraceMaxEvents
	}
	return &out
}

// TLSConfig is per-server TLS for remote entries.
type TLSConfig struct {
	CAFile   string `toml:"ca_file"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
	Insecure bool   `toml:"insecure"`
}

// OAuthConfig overrides for daemon-held OAuth (all optional; DCR by default).
type OAuthConfig struct {
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	Scope        string `toml:"scope"`
	CallbackPort int    `toml:"callback_port"` // default 8378
	// RedirectURI template; "{port}" expands to the bound callback port.
	// Empty = default loopback URL for the bound port.
	RedirectURI string `toml:"redirect_uri"`
	Flow        string `toml:"flow"`       // browser (default) | device | token
	GrantType   string `toml:"grant_type"` // "" or "client_credentials"
	Timeout     int    `toml:"timeout"`    // device poll cap, seconds (default 300)
}

// ToolsPolicy filters a server's tools (C1). Glob lists (* and ?, same engine
// as lazy-mcp). Deny always wins; deny ⇒ invisible at discovery and blocked
// at invoke. Nil policy or empty allow = allow all.
type ToolsPolicy struct {
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
}

// Load reads dir/oxyp.toml, merges auto-discovered connectors, validates.
// Never fails on a bad entry — marks ConfigError instead. Fails only if the
// file itself is unreadable or unparseable.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, "oxyp.toml")
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	if cfg.Daemon.Token != "" {
		tok, err := expandValue(cfg.Daemon.Token)
		if err != nil {
			return nil, fmt.Errorf("load %s: daemon.token: %v", path, err)
		}
		if envRefRe.MatchString(tok) || fileRefRe.MatchString(tok) {
			return nil, fmt.Errorf("load %s: daemon.token has unset ${VAR}", path)
		}
		cfg.Daemon.Token = tok
	}
	if msg := ValidateDaemon(&cfg.Daemon); msg != "" {
		return nil, fmt.Errorf("load %s: %s", path, msg)
	}
	if cfg.Daemon.Addr == "" {
		cfg.Daemon.Addr = "127.0.0.1:8379"
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]ServerEntry{}
	}
	connectors := cfg.Daemon.ConnectorsDir
	if connectors == "" {
		connectors = filepath.Join(dir, "connectors")
	}
	mergeConnectors(&cfg, connectors)
	for name, s := range cfg.Servers {
		s.Name = name
		if msg := expandServerSecrets(&s); msg != "" {
			s.ConfigError = msg
			cfg.Servers[name] = s
			continue
		}
		s.ConfigError = validateServer(&s)
		cfg.Servers[name] = s
	}
	return &cfg, nil
}

// mergeConnectors adds executable files in connectors/ as servers by filename
// (stem = name). Explicit TOML entries with the same name win. Files must be
// one-file MCP servers over stdio (user decision: option A, any language).
func mergeConnectors(cfg *Config, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no connectors dir = fine
	}
	known := map[string]bool{}
	for name := range cfg.Servers {
		known[strings.ToLower(name)] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if name == "" || known[strings.ToLower(name)] {
			continue
		}
		full := filepath.Join(dir, e.Name())
		st, err := os.Stat(full)
		if err != nil || st.Mode()&0o111 == 0 {
			continue // must be executable
		}
		known[strings.ToLower(name)] = true
		cfg.Servers[name] = ServerEntry{
			Description:   fmt.Sprintf("Auto-discovered connector %s", e.Name()),
			Command:       []string{full},
			FromConnector: full,
		}
	}
}

func validateServer(s *ServerEntry) string {
	if s.Name == "" {
		return `"name" is required`
	}
	if s.URL != "" {
		if len(s.Command) != 0 {
			return fmt.Sprintf("server %q: url and command are exclusive", s.Name)
		}
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			return fmt.Sprintf("server %q: url must start with http(s)://", s.Name)
		}
		if s.Transport != "" && s.Transport != "sse" {
			return fmt.Sprintf("server %q: transport must be \"sse\" or empty (auto)", s.Name)
		}
		for k, v := range s.Headers {
			if strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
				return fmt.Sprintf("server %q: bad header %q", s.Name, k)
			}
		}
		if s.Cwd != "" || s.InheritEnv != nil {
			return fmt.Sprintf("server %q: cwd/inherit_env is local-only", s.Name)
		}
	} else {
		if s.Transport != "" {
			return fmt.Sprintf("server %q: transport is remote-only", s.Name)
		}
		if len(s.Headers) != 0 {
			return fmt.Sprintf("server %q: headers is remote-only", s.Name)
		}
		if s.TLS != nil {
			return fmt.Sprintf("server %q: tls is remote-only", s.Name)
		}
		if s.Auth != "" || s.OAuth != nil {
			return fmt.Sprintf("server %q: auth/oauth is remote-only", s.Name)
		}
		if len(s.Command) == 0 {
			return fmt.Sprintf("server %q: no command (and no matching connector file)", s.Name)
		}
	}
	if s.Auth != "" && s.Auth != "oauth" {
		return fmt.Sprintf("server %q: auth must be \"oauth\" or empty", s.Name)
	}
	if s.OAuth != nil && s.OAuth.CallbackPort < 0 {
		return fmt.Sprintf("server %q: oauth.callback_port must be >= 0", s.Name)
	}
	if s.OAuth != nil && s.OAuth.Flow != "" &&
		s.OAuth.Flow != "browser" && s.OAuth.Flow != "device" && s.OAuth.Flow != "token" {
		return fmt.Sprintf("server %q: oauth.flow must be browser|device|token", s.Name)
	}
	if s.OAuth != nil && s.OAuth.GrantType != "" && s.OAuth.GrantType != "client_credentials" {
		return fmt.Sprintf("server %q: oauth.grant_type must be \"client_credentials\" or empty", s.Name)
	}
	if s.OAuth != nil && s.OAuth.GrantType == "client_credentials" &&
		(s.OAuth.ClientID == "" || s.OAuth.ClientSecret == "") {
		return fmt.Sprintf("server %q: oauth.grant_type client_credentials needs client_id and client_secret", s.Name)
	}
	if s.OAuth != nil && s.OAuth.Timeout < 0 {
		return fmt.Sprintf("server %q: oauth.timeout must be >= 0", s.Name)
	}
	if s.TLS != nil {
		t := s.TLS
		if (t.CertFile == "") != (t.KeyFile == "") {
			return fmt.Sprintf("server %q: tls cert_file and key_file must both be set", s.Name)
		}
		for _, f := range []string{t.CAFile, t.CertFile, t.KeyFile} {
			if f != "" {
				if st, err := os.Stat(f); err != nil || st.IsDir() {
					return fmt.Sprintf("server %q: tls file unreadable: %s", s.Name, f)
				}
			}
		}
	}
	if s.Tools != nil {
		for i, p := range s.Tools.Allow {
			if strings.TrimSpace(p) == "" {
				return fmt.Sprintf("server %q: tools.allow[%d] must not be empty", s.Name, i)
			}
		}
		for i, p := range s.Tools.Deny {
			if strings.TrimSpace(p) == "" {
				return fmt.Sprintf("server %q: tools.deny[%d] must not be empty", s.Name, i)
			}
		}
	}
	switch s.Lifecycle {
	case "", "lazy", "eager", "keep-alive", "lazy-keep-alive":
	default:
		return fmt.Sprintf("server %q: lifecycle must be lazy|eager|keep-alive|lazy-keep-alive", s.Name)
	}
	if s.IdleTimeout != nil && *s.IdleTimeout < 0 {
		return fmt.Sprintf("server %q: idle_timeout must be >= 0", s.Name)
	}
	if s.OutputGuard != nil {
		if s.OutputGuard.MaxBytes < 0 || s.OutputGuard.MaxLines < 0 {
			return fmt.Sprintf("server %q: output_guard limits must be >= 0", s.Name)
		}
	}
	if s.Trace != nil && s.Trace.File != "" {
		return fmt.Sprintf("server %q: trace.file is daemon-level only", s.Name)
	}
	if s.Trace != nil && (s.Trace.MaxBytes < 0 || s.Trace.MaxEvents < 0) {
		return fmt.Sprintf("server %q: trace limits must be >= 0", s.Name)
	}
	return ""
}

// ValidateDaemon checks daemon-wide blocks (called from Load).
// A non-loopback bind with an empty token is a Load error.
func ValidateDaemon(d *DaemonConfig) string {
	if d.OutputGuard != nil {
		if d.OutputGuard.MaxBytes < 0 || d.OutputGuard.MaxLines < 0 {
			return "daemon.output_guard limits must be >= 0"
		}
	}
	if d.Trace != nil && (d.Trace.MaxBytes < 0 || d.Trace.MaxEvents < 0) {
		return "daemon.trace limits must be >= 0"
	}
	if d.IdleTimeout != nil && *d.IdleTimeout < 0 {
		return "daemon.idle_timeout must be >= 0"
	}
	if d.Token == "" && d.Addr != "" && !isLoopbackAddr(d.Addr) {
		return "daemon.token is required for non-loopback addr"
	}
	return ""
}

// isLoopbackAddr reports whether addr (host:port) binds loopback only.
// Unparseable hosts fail closed (not loopback). Empty host (":port",
// "0.0.0.0", "::") is non-loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
