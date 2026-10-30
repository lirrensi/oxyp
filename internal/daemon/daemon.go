// Package daemon owns downstream MCP server processes.
//
// Design (user decisions 2026-10-28):
//   - Cold by default: a server spawns only when named (list/call), never
//     all-at-once. Health pings cover connected servers only.
//   - Bad config entries never break the daemon: they report unhealthy.
//   - Spawn/health/shutdown patterns stolen from mozilla-ai/mcpd
//     (internal/daemon): stderr piping, init timeout, ping loop, close timeout.
package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"oxyp/internal/config"
	"oxyp/internal/metacache"
	"oxyp/internal/oauth"
	"oxyp/internal/perms"
	"oxyp/internal/ptrace"
	"oxyp/internal/redact"
	"oxyp/internal/remhttp"
	"oxyp/internal/search"
)

const (
	DefaultInitTimeout     = 30 * time.Second
	DefaultHealthInterval  = 10 * time.Second
	DefaultHealthTimeout   = 3 * time.Second
	DefaultShutdownTimeout = 5 * time.Second
	DefaultRequestTimeout  = 10 * time.Second
	// RemoteToolsTTL bounds how long remote discovery results are trusted.
	// Streamable HTTP has no server-push channel in mcp-go v1.4.0
	// (WithContinuousListening is deprecated + pins legacy protocol; the
	// subscriptions/listen flow it points to does not exist yet), so remote
	// list_changed notifications never arrive. Local children keep push via
	// the OnNotification handler in spawn and need no TTL.
	RemoteToolsTTL = 5 * time.Minute
)

// Health is the last known state of one server.
type Health struct {
	Healthy     bool
	LastChecked time.Time
	Latency     time.Duration
	Error       string
	// AuthRequired holds the authorize URL when the server is reachable
	// but needs OAuth (lazy-mcp auth_required — NOT an error).
	AuthRequired string
}

// AuthNeededError signals interactive OAuth is required. Callers surface
// the URL instead of treating it as a failure.
type AuthNeededError struct {
	Server string
	URL    string
}

func (e *AuthNeededError) Error() string {
	if e.URL == "" {
		return fmt.Sprintf("server %q needs OAuth: run: oxyp login %s", e.Server, e.Server)
	}
	return fmt.Sprintf("server %q needs OAuth: open %s", e.Server, e.URL)
}

// pendingLogin is a started browser flow awaiting callback completion
// (guarded by managed.mu).
type pendingLogin struct {
	handler  *transport.OAuthHandler
	txn      transport.AuthorizationTransaction
	state    string
	verifier string
}

// managed is one downstream server: config + lazy connection.
type managed struct {
	cfg config.ServerEntry
	mu  sync.Mutex
	// gen is the config generation (under m.mu): Reload bumps it for
	// modified entries so an in-flight spawn can discard stale results.
	gen    int
	client client.MCPClient
	// pending holds a started browser flow for manual --complete.
	// connecting dedupes concurrent cold starts (lazy-mcp's managed.connecting).
	pending    *pendingLogin
	connecting chan struct{}
	// spawnErr carries the last spawner's terminal error to woken
	// waiters (under m.mu). Consumed on take; cleared on next spawn.
	spawnErr error
	// inFlight counts active tool calls; the idle sweeper never stops
	// a server with work running.
	inFlight int
	// lastUsed is the last completed activity (spawn counts).
	lastUsed time.Time

	healthMu sync.RWMutex
	health   *Health

	toolsMu sync.RWMutex
	tools   []mcp.Tool // discovery cache
	// toolsCachedAt is when the memory tools cache was last filled
	// (live or disk); remote entries revalidate past RemoteToolsTTL.
	toolsCachedAt time.Time
	resMu         sync.RWMutex
	res           []mcp.Resource
	// instructions is captured at init and cached with tools.
	instructions string
	prompts      []mcp.Prompt
}

// Daemon holds all downstream servers. Create with New.
type Daemon struct {
	mu       sync.RWMutex
	servers  map[string]*managed
	log      *slog.Logger
	initTO   time.Duration
	healthIv time.Duration
	healthTO time.Duration
	reqTO    time.Duration
	// oauthDir is <oxypdir>/oauth for token stores. "" disables OAuth.
	oauthDir string
	// cache persists tool metadata to disk (nil = disabled, e.g. tests).
	cache *metacache.Cache
	// daemonCfg holds daemon-wide settings (idle default).
	daemonCfg config.DaemonConfig
	// tracer emits metadata-only protocol events (nil = off).
	tracer *ptrace.Tracer
	// spawnCtx is the daemon-lifetime context for downstream clients:
	// stdio children and SSE streams die with it, never with a per-RPC
	// request context. Cancelled by Close.
	spawnCtx context.Context
	// spawnStop cancels spawnCtx; guarded by mu.
	spawnStop context.CancelFunc
}

// SetTracer attaches a protocol tracer (nil disables).
func (d *Daemon) SetTracer(t *ptrace.Tracer) {
	d.mu.Lock()
	d.tracer = t
	d.mu.Unlock()
}

func (d *Daemon) trace() *ptrace.Tracer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.tracer
}

func (d *Daemon) traceFor(m *managed) bool {
	if d.trace() == nil {
		return false
	}
	cfg := m.getCfg()
	dc := d.DaemonConfig()
	tc := cfg.EffectiveTrace(dc)
	return tc != nil && tc.Enabled
}

// New builds a daemon from config. Disabled servers are skipped;
// misconfigured ones are kept and report unhealthy (soft-fail).
func New(cfg *config.Config, log *slog.Logger) *Daemon {
	return NewWithOAuthDir(cfg, log, "")
}

// DaemonConfig returns a deep copy of the daemon-wide settings.
func (d *Daemon) DaemonConfig() config.DaemonConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.daemonCfg.DeepCopy()
}

// NewWithOAuthDir also sets the token store dir ("" = OAuth unavailable).
func NewWithOAuthDir(cfg *config.Config, log *slog.Logger, oauthDir string) *Daemon {
	return NewWithDirs(cfg, log, oauthDir, "")
}

// NewWithDirs also sets the metadata cache path ("" = cache disabled).
func NewWithDirs(cfg *config.Config, log *slog.Logger, oauthDir, cachePath string) *Daemon {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	d := &Daemon{
		servers:   map[string]*managed{},
		log:       log,
		initTO:    DefaultInitTimeout,
		healthIv:  DefaultHealthInterval,
		healthTO:  DefaultHealthTimeout,
		reqTO:     DefaultRequestTimeout,
		oauthDir:  oauthDir,
		daemonCfg: cfg.Daemon,
	}
	d.spawnCtx, d.spawnStop = context.WithCancel(context.Background())
	if cachePath != "" {
		d.cache = metacache.New(cachePath)
	}
	fps := map[string]string{}
	for name, s := range cfg.Servers {
		if !s.IsEnabled() {
			continue
		}
		s.Name = name // map key wins; test-built entries may omit Name
		m := &managed{cfg: s}
		if s.ConfigError != "" {
			m.health = &Health{LastChecked: time.Now(), Error: "config: " + s.ConfigError}
		}
		d.servers[name] = m
		fps[name] = metacache.Fingerprint(s)
	}
	if d.cache != nil {
		d.cache.DropStale(fps)
	}
	return d
}

// Names returns sorted configured server names (excludes disabled).
func (d *Daemon) Names() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.servers))
	for n := range d.servers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Entry returns a FRESH COPY of the config entry for a server, or nil.
// The caller may read the copy lock-free; the live entry never escapes.
func (d *Daemon) Entry(name string) *config.ServerEntry {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return nil
	}
	cfg := m.getCfg()
	return &cfg
}

// getCfg returns a deep copy of the managed config under m.mu.
func (m *managed) getCfg() config.ServerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCfgLocked()
}

// getCfgLocked reads m.cfg; the caller must hold m.mu.
func (m *managed) getCfgLocked() config.ServerEntry {
	return m.cfg.DeepCopy()
}

// IsConnected reports whether the server has a live client (memory only,
// never spawns). For the list path: connection state, not health history.
func (d *Daemon) IsConnected(name string) bool {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client != nil
}

// CachedInstructions returns the init-time instructions captured in memory,
// or false when absent. It never spawns: failed/idle servers report none.
func (d *Daemon) CachedInstructions(name string) (string, bool) {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return "", false
	}
	m.resMu.RLock()
	defer m.resMu.RUnlock()
	return m.instructions, m.instructions != ""
}

// HealthOf returns the last health record, or nil if never checked/connected.
func (d *Daemon) HealthOf(name string) *Health {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return nil
	}
	m.healthMu.RLock()
	defer m.healthMu.RUnlock()
	return m.health
}

// get returns the managed server or an error (unknown or misconfigured).
func (d *Daemon) get(name string) (*managed, error) {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("server %q not found", name)
	}
	if cfgErr := m.getCfg().ConfigError; cfgErr != "" {
		return nil, fmt.Errorf("server %q misconfigured: %s", name, cfgErr)
	}
	return m, nil
}

// SearchHit is one ranked tool match.
type SearchHit struct {
	Server      string
	Name        string
	Description string
	Score       int
}

// SearchTools ranks tools across servers by words. Only servers with
// cached metadata (memory or disk) are searched — search never spawns.
// Denied tools are excluded (C1). regex=true switches to unranked
// pattern match. Returns hits + total + next offset (-1 = done).
func (d *Daemon) SearchTools(name, query string, regex bool, limit, offset int) (hits []SearchHit, total, next int, err error) {
	var cands []search.Candidate
	collect := func(srvName string, m *managed) {
		e := m.getCfg()
		tools := m.cachedTools()
		if tools == nil && d.cache != nil {
			if t, ok := d.cache.Get(srvName, metacache.Fingerprint(e)); ok {
				tools = t
			}
		}
		if tools == nil {
			return // uncached: skip, never spawn
		}
		for _, t := range tools {
			if perms.Resolve(&e, t.Name).Action != "allow" {
				continue
			}
			cands = append(cands, search.Candidate{
				Server: srvName, ServerDescription: e.Description,
				Name: t.Name, Description: t.Description,
			})
		}
	}
	if name != "" {
		m, gerr := d.get(name)
		if gerr != nil {
			return nil, 0, 0, gerr
		}
		collect(name, m)
	} else {
		d.mu.RLock()
		names := make([]string, 0, len(d.servers))
		for n := range d.servers {
			names = append(names, n)
		}
		d.mu.RUnlock()
		sort.Strings(names)
		for _, n := range names {
			d.mu.RLock()
			m := d.servers[n]
			d.mu.RUnlock()
			if m == nil || m.getCfg().ConfigError != "" {
				continue
			}
			collect(n, m)
		}
	}
	var ranked []search.Result
	if regex {
		ranked, err = search.Regex(query, cands)
	} else {
		ranked = search.Rank(query, cands)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	page, next := search.Page(ranked, limit, offset)
	for _, r := range page {
		hits = append(hits, SearchHit{Server: r.Server, Name: r.Name,
			Description: r.Description, Score: r.Score})
	}
	return hits, len(ranked), next, nil
}

// cachedTools returns memory-cached tools or nil (no spawn, no disk).
func (m *managed) cachedTools() []mcp.Tool {
	m.toolsMu.RLock()
	defer m.toolsMu.RUnlock()
	return m.tools
}

// clientFor returns a connected client, spawning lazily on first use.
// Concurrent cold starts dedupe onto one spawn. The spawn itself runs on
// the daemon-lifetime spawnCtx (never the per-RPC request ctx) so a
// completed HTTP response cannot kill the child or SSE stream; callers
// still cancel their wait via ctx.Done.
func (d *Daemon) clientFor(ctx context.Context, m *managed) (client.MCPClient, error) {
	c, err := d.clientForGen(ctx, m, 0)
	return c, err
}

// clientForGen is clientFor with a bounded generation-mismatch retry:
// at most one re-spawn against the new config (retries=1).
func (d *Daemon) clientForGen(ctx context.Context, m *managed, retries int) (client.MCPClient, error) {
	for {
		m.mu.Lock()
		if m.client != nil {
			c := m.client
			m.mu.Unlock()
			return c, nil
		}
		if m.connecting == nil {
			if m.spawnErr != nil {
				// A spawner already failed for this cohort: report its
				// error instead of stampeding. Consumed on take, so a
				// later caller retries fresh (recovery possible).
				err := m.spawnErr
				m.spawnErr = nil
				m.mu.Unlock()
				return nil, err
			}
			break
		}
		ch := m.connecting
		m.mu.Unlock()
		select {
		case <-ch:
			// Loop back: take the client, take the spawner's
			// error, or become the spawner.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ch := make(chan struct{})
	m.connecting = ch
	gen := m.gen
	m.mu.Unlock()

	c, err := d.spawn(ctx, m, gen)

	m.mu.Lock()
	m.connecting = nil
	if err == nil {
		if m.gen != gen && retries < 1 {
			// Reload replaced the config mid-spawn: discard the stale
			// client and retry once against the new config.
			close(ch)
			m.mu.Unlock()
			_ = c.Close()
			return d.clientForGen(ctx, m, retries+1)
		}
		if m.gen == gen {
			m.client = c
			m.lastUsed = time.Now()
		} else {
			// Second mismatch: drop the stale client, report the race.
			close(ch)
			m.mu.Unlock()
			_ = c.Close()
			return nil, fmt.Errorf("server %q: config changed during connect; retry", m.getCfg().Name)
		}
	} else {
		m.spawnErr = err
	}
	close(ch)
	m.mu.Unlock()
	return c, err
}

// spawn connects to the downstream server and runs MCP initialize.
// Local entries spawn a stdio child (mcpd pattern: stderr piping, bounded
// initialize); remote entries open Streamable HTTP, falling back to legacy
// SSE when the server signals it (mcp-go ErrLegacySSEServer) or when
// transport: "sse" is configured. Both stay lazy.
func (d *Daemon) spawn(ctx context.Context, m *managed, gen int) (client.MCPClient, error) {
	cfg := m.getCfg()
	log := d.log.With("server", cfg.Name)
	// Downstream lifetime binds to the daemon, not the triggering RPC:
	// stdio children (exec.CommandContext) and SSE streams would die
	// when the request ctx is cancelled after ServeHTTP returns.
	d.mu.RLock()
	lifetime := d.spawnCtx
	d.mu.RUnlock()
	var c *client.Client
	var err error
	if cfg.IsRemote() {
		c, err = d.dialRemote(m)
		if err != nil {
			return nil, fmt.Errorf("dial %q: %w", cfg.Name, err)
		}
	} else {
		argv := cfg.Command
		c, err = client.NewStdioMCPClientWithOptions(argv[0], childEnv(cfg), argv[1:],
			transport.WithCommandFunc(commandFuncFor(cfg)))
		if err != nil {
			return nil, fmt.Errorf("spawn %q: %w", cfg.Name, err)
		}
		if stderr, ok := client.GetStderr(c); ok {
			go pipeStderr(lifetime, log, stderr)
		}
	}

	res, err := d.startAndInit(lifetime, ctx, c)
	if err != nil && cfg.IsRemote() && cfg.Transport != "sse" &&
		errors.Is(err, transport.ErrLegacySSEServer) {
		// Legacy server: retry the whole connection over SSE.
		_ = c.Close()
		log.Info("streamable HTTP rejected, falling back to SSE")
		if c, err = d.dialRemoteSSE(m); err != nil {
			return nil, fmt.Errorf("dial %q via SSE: %w", cfg.Name, err)
		}
		res, err = d.startAndInit(lifetime, ctx, c)
	}
	if err != nil {
		_ = c.Close()
		// OAuth 401: reachable but needs login. Auto-detect (#9): 401 +
		// resource_metadata (WWW-Authenticate or mcp-go's OAuth errors) on
		// ANY remote without explicit auth triggers the auth-required path
		// (AuthNeededError + auth_required health + login works). Explicit
		// auth="oauth" forces it regardless of metadata presence.
		if cfg.IsRemote() && (cfg.Auth == "oauth" || hasResourceMetadata(err)) &&
			(errors.Is(err, transport.ErrOAuthAuthorizationRequired) ||
				errors.Is(err, transport.ErrAuthorizationRequired)) {
			m.setHealth(&Health{LastChecked: time.Now(), AuthRequired: "run: oxyp login " + cfg.Name})
			return nil, &AuthNeededError{Server: cfg.Name}
		}
		if cfg.IsRemote() {
			// One cheap probe turns opaque connect failures into setup
			// hints (HTML page, bad status, refused/TLS). No retry loop.
			if hint := probeRemote(cfg, d.reqTO); hint != "" {
				err = fmt.Errorf("%w (%s)", err, hint)
			}
		}
		return nil, fmt.Errorf("initialize %q: %w", cfg.Name, err)
	}
	// Capture server instructions at init; cached with tools below.
	if res.Instructions != "" {
		m.resMu.Lock()
		m.instructions = res.Instructions
		m.resMu.Unlock()
	}
	log.Info("server ready", "upstream", res.ServerInfo.Name, "version", res.ServerInfo.Version)
	// tools/list_changed → drop discovery cache. stdio + SSE push over the
	// live connection; Streamable HTTP has no push channel in mcp-go
	// v1.4.0 (see RemoteToolsTTL), so remotes also revalidate by TTL.
	srvName := cfg.Name
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		switch n.Method {
		case mcp.MethodNotificationToolsListChanged:
			d.log.Info("tools changed upstream, invalidating cache", "server", srvName)
			d.InvalidateTools(srvName)
		case mcp.MethodNotificationResourcesListChanged,
			mcp.MethodNotificationPromptsListChanged:
			d.log.Info("resources/prompts changed upstream, invalidating cache", "server", srvName)
			d.InvalidateResources(srvName)
		}
	})
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
	return c, nil
}

// dialRemote opens a Streamable HTTP or SSE client with oxyp's hardened
// http.Clients (redirect policy + per-server TLS) and optional OAuth.
// Streaming uses a Timeout:0 client (idle SSE streams and event-stream
// POST bodies are long-lived; http.Client.Timeout covers body reads, so a
// 10s timeout would kill them). Per-RPC timeouts come from request
// contexts (reqTO) and WithResponseTimeout instead — mcp-go builds every
// request with http.NewRequestWithContext, so cancellations still fire.
func (d *Daemon) dialRemote(m *managed) (*client.Client, error) {
	cfg := m.getCfg()
	_, streamClient, ocfg, hasOAuth, err := d.remoteStack(m)
	if err != nil {
		return nil, err
	}
	if cfg.Transport == "sse" {
		opts := []transport.ClientOption{
			transport.WithHeaders(cfg.Headers),
			transport.WithResponseTimeout(d.reqTO),
			transport.WithHTTPClient(streamClient),
		}
		if hasOAuth {
			opts = append(opts, transport.WithOAuth(ocfg))
		}
		return client.NewSSEMCPClient(cfg.URL, opts...)
	}
	opts := []transport.StreamableHTTPCOption{
		transport.WithHTTPHeaders(cfg.Headers),
		transport.WithHTTPTimeout(0),
		transport.WithHTTPBasicClient(streamClient),
	}
	if hasOAuth {
		opts = append(opts, transport.WithHTTPOAuth(ocfg))
	}
	return client.NewStreamableHttpClient(cfg.URL, opts...)
}

// dialRemoteSSE forces the SSE transport (explicit config or auto-fallback).
func (d *Daemon) dialRemoteSSE(m *managed) (*client.Client, error) {
	cfg := m.getCfg()
	_, streamClient, ocfg, hasOAuth, err := d.remoteStack(m)
	if err != nil {
		return nil, err
	}
	opts := []transport.ClientOption{
		transport.WithHeaders(cfg.Headers),
		transport.WithResponseTimeout(d.reqTO),
		transport.WithHTTPClient(streamClient),
	}
	if hasOAuth {
		opts = append(opts, transport.WithOAuth(ocfg))
	}
	return client.NewSSEMCPClient(cfg.URL, opts...)
}
func (d *Daemon) remoteStack(m *managed) (rpcClient, streamClient *http.Client, ocfg transport.OAuthConfig, hasOAuth bool, err error) {
	cfg := m.getCfg()
	tlsCfg := remhttp.TransportTLS{}
	if cfg.TLS != nil {
		tlsCfg = remhttp.TransportTLS{
			CAFile: cfg.TLS.CAFile, CertFile: cfg.TLS.CertFile,
			KeyFile: cfg.TLS.KeyFile, Insecure: cfg.TLS.Insecure,
		}
	}
	rpcClient = remhttp.NewClient(d.reqTO, tlsCfg)
	streamClient = remhttp.NewClient(0, tlsCfg)
	useOAuth := cfg.IsRemote() && d.oauthDir != "" && cfg.Auth == "oauth"
	if cfg.OAuth != nil && cfg.OAuth.Flow == "token" {
		useOAuth = false // token flow: static headers are the auth
	} else if cfg.IsRemote() && d.oauthDir != "" && cfg.Auth == "" {
		// Auto-detect (#9): never force OAuth on plain remotes — that
		// would turn every unauthenticated probe into AuthNeeded before
		// any bytes hit the wire. Attach the handler only once tokens
		// exist (post-login); the first 401 + resource_metadata maps to
		// auth_required via hasResourceMetadata in spawn.
		useOAuth = d.hasStoredToken(m)
	}
	if !useOAuth {
		return rpcClient, streamClient, transport.OAuthConfig{}, false, nil
	}
	store, serr := oauth.NewStore(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL))
	if serr != nil {
		return nil, nil, transport.OAuthConfig{}, false, fmt.Errorf("oauth store: %w", serr)
	}
	ocfg = transport.OAuthConfig{
		RedirectURI: OAuthCallbackURL(cfg),
		TokenStore:  store,
		PKCEEnabled: true,
		HTTPClient:  rpcClient,
	}
	if cfg.OAuth != nil {
		ocfg.ClientID = cfg.OAuth.ClientID
		ocfg.ClientSecret = cfg.OAuth.ClientSecret
		if cfg.OAuth.Scope != "" {
			ocfg.Scopes = []string{cfg.OAuth.Scope}
		}
	}
	// DCR-registered credentials (slice B persists them): prefer stored
	// registration when the config carries no static client_id.
	if ocfg.ClientID == "" && d.oauthDir != "" {
		if reg, rerr := oauth.LoadClientReg(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL)); rerr == nil {
			oauth.ApplyClientReg(&ocfg, reg)
		}
	}
	return rpcClient, streamClient, ocfg, true, nil
}
func (d *Daemon) hasStoredToken(m *managed) bool {
	if d.oauthDir == "" {
		return false
	}
	cfg := m.getCfg()
	store, err := oauth.NewStore(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL))
	if err != nil {
		return false
	}
	tok, err := store.GetToken(context.Background())
	return err == nil && tok != nil && tok.AccessToken != ""
}

// OAuthCallbackURL builds the loopback redirect URI for a server.
// oauth.redirect_uri template wins ({port} → 8378 default or callback_port
// when no live port is known); CallbackPort only sets the default port.
// Live login flows re-expand the template with the bound ephemeral port.
func OAuthCallbackURL(cfg config.ServerEntry) string {
	port := 8378
	if cfg.OAuth != nil && cfg.OAuth.CallbackPort != 0 {
		port = cfg.OAuth.CallbackPort
	}
	if cfg.OAuth != nil && cfg.OAuth.RedirectURI != "" {
		return oauth.ExpandRedirectURI(cfg.OAuth.RedirectURI, port)
	}
	return fmt.Sprintf("http://localhost:%d/callback", port)
}

// hasResourceMetadata reports whether err carries an RFC 9728
// resource_metadata hint: mcp-go's OAuth-specific error (only produced with
// an OAuth handler + 401 discovery), or a resource_metadata parameter in
// the message. Plain 401s without it stay plain errors (no false login
// prompts on basic-auth servers).
func hasResourceMetadata(err error) bool {
	if errors.Is(err, transport.ErrOAuthAuthorizationRequired) {
		return true
	}
	var oauthErr *transport.OAuthAuthorizationRequiredError
	if errors.As(err, &oauthErr) {
		return true
	}
	var authErr *transport.AuthorizationRequiredError
	if errors.As(err, &authErr) {
		return authErr.ResourceMetadataURL != ""
	}
	return strings.Contains(strings.ToLower(err.Error()), "resource_metadata")
}

// startAndInit starts the transport on the daemon-lifetime ctx (child
// process / SSE stream survival) and bounds only initialize on the
// request ctx: cancelling the RPC still aborts a hung init, but a
// completed RPC never kills the connection.
func (d *Daemon) startAndInit(lifetime, reqCtx context.Context, c *client.Client) (*mcp.InitializeResult, error) {
	// Start is idempotent; stdio/StreamableHTTP self-start, SSE requires it.
	if err := c.Start(lifetime); err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(reqCtx, d.initTO)
	defer cancel()
	return c.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "oxyp", Version: "0.1.0"},
		},
	})
}

// pipeStderr forwards child stderr to the daemon log (mcpd pattern).
// Runs on the daemon-lifetime ctx; stderr lines are redacted.
func pipeStderr(ctx context.Context, log *slog.Logger, r io.Reader) {
	rd := bufio.NewReader(r)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := rd.ReadString('\n')
		if ctx.Err() != nil {
			return
		}
		if err != nil && err != io.EOF {
			log.Error("stderr read error", "err", redact.Body(err.Error()))
			return
		}
		if line != "" {
			log.Info("upstream stderr", "line", redact.Body(line[:min(len(line), 500)]))
		}
		if err == io.EOF {
			return
		}
	}
}

func (m *managed) setHealth(h *Health) {
	m.healthMu.Lock()
	m.health = h
	m.healthMu.Unlock()
}

// ListTools discovers (and caches) downstream tools. Order: memory →
// disk (cold, no spawn) → live (spawns cold servers). Disk hits never
// connect; live results refresh both caches.
// On session expiry (remote 404) it re-initializes once and retries —
// lazy-mcp's 404 policy, via mcp-go's ErrSessionTerminated.
// Remote memory hits older than RemoteToolsTTL revalidate live once
// (Streamable HTTP has no push channel); failure serves stale.
func (d *Daemon) ListTools(ctx context.Context, name string) ([]mcp.Tool, error) {
	m, err := d.get(name)
	if err != nil {
		return nil, err
	}
	cfg := m.getCfg()
	m.toolsMu.RLock()
	memTools, memAt := m.tools, m.toolsCachedAt
	m.toolsMu.RUnlock()
	if memTools != nil {
		if !cfg.IsRemote() || time.Since(memAt) <= RemoteToolsTTL {
			return memTools, nil
		}
		// Stale remote: one live revalidation; failure serves stale.
		if tools, rerr := d.listToolsOnce(ctx, m); rerr == nil {
			d.storeToolsGen(name, m, cfg, tools, genOf(m))
			m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
			return tools, nil
		}
		d.log.Info("remote revalidation failed, serving stale tools", "server", name)
		return memTools, nil
	}

	fp := metacache.Fingerprint(cfg)
	if d.cache != nil {
		if tools, ok := d.cache.Get(name, fp); ok {
			m.toolsMu.Lock()
			m.tools = tools
			m.toolsCachedAt = time.Now()
			m.toolsMu.Unlock()
			return tools, nil
		}
	}

	gen := genOf(m)
	tools, err := d.listToolsOnce(ctx, m)
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		if cfg.IsRemote() && errors.Is(err, transport.ErrSessionTerminated) {
			d.log.Info("session expired, re-initializing", "server", name)
			d.resetClient(m)
			tools, err = d.listToolsOnce(ctx, m)
		}
	}
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		d.recordFailure(m, err)
		return nil, err
	}
	d.storeToolsGen(name, m, cfg, tools, gen)
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
	return tools, nil
}

// genOf reads the config generation (Reload bumps it on modify).
func genOf(m *managed) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// storeTools fills the memory + disk discovery caches with a timestamp.
func (d *Daemon) storeTools(name string, m *managed, cfg config.ServerEntry, tools []mcp.Tool) {
	d.storeToolsGen(name, m, cfg, tools, genOf(m))
}

// storeToolsGen skips the store when the config generation changed since
// the live call started (Reload cleared the cache; stale results must not
// repopulate it). wantGen is the generation captured before the live call.
func (d *Daemon) storeToolsGen(name string, m *managed, cfg config.ServerEntry, tools []mcp.Tool, wantGen int) {
	if genOf(m) != wantGen {
		return
	}
	m.toolsMu.Lock()
	m.tools = tools
	m.toolsCachedAt = time.Now()
	m.toolsMu.Unlock()
	if d.cache != nil {
		d.cache.Put(name, metacache.Fingerprint(cfg), tools)
	}
}

func (d *Daemon) listToolsOnce(ctx context.Context, m *managed) ([]mcp.Tool, error) {
	c, err := d.clientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.reqTO)
	defer cancel()
	cfg := m.getCfg()
	start := time.Now()
	res, err := c.ListTools(reqCtx, mcp.ListToolsRequest{})
	if d.traceFor(m) {
		outcome, n := "ok", 0
		if err != nil {
			outcome = "error:" + errClass(err)
		} else if res != nil {
			n = len(res.Tools)
		}
		d.trace().Emit(ptrace.Event{Server: cfg.Name, Direction: "out",
			Method: "tools/list", LatencyMs: time.Since(start).Milliseconds(),
			Outcome: outcome, Note: fmt.Sprintf("tools=%d", n)})
	}
	if err != nil {
		// mcp-go unmarshals the whole tools[] strictly: one malformed
		// tool schema poisons the list. No raw path exists to salvage
		// the good tools, so fail with a clear pointer instead of Go
		// internals. (opencode retries dropping outputSchema; we can't
		// reach the raw bytes through the Client API.)
		if strings.Contains(err.Error(), "unmarshal") {
			return nil, fmt.Errorf("server %q returned an unparseable tool list (likely a malformed inputSchema on one tool): %v", cfg.Name, err)
		}
		return nil, err
	}
	return res.Tools, nil
}

// errClass buckets an error for tracing without leaking details.
func errClass(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "connection refused"):
		return "refused"
	case strings.Contains(msg, "unmarshal"),
		strings.Contains(msg, "decode"):
		return "protocol"
	case errors.Is(err, transport.ErrSessionTerminated):
		return "session-expired"
	case errors.Is(err, transport.ErrOAuthAuthorizationRequired),
		errors.Is(err, transport.ErrAuthorizationRequired):
		return "auth-required"
	default:
		return "error"
	}
}

// resetClient drops the cached connection so the next use reconnects fresh.
// Also clears the sticky spawn error: a reset means "try again", e.g.
// after login saved fresh tokens — serving the pre-reset failure would
// wedge the server on its old error.
func (d *Daemon) resetClient(m *managed) {
	name := m.getCfg().Name
	m.mu.Lock()
	c := m.client
	m.client = nil
	m.spawnErr = nil
	m.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	d.InvalidateTools(name)
	d.InvalidateResources(name)
}

// mapAuthErr converts mid-session OAuth 401s (any RPC, not just spawn)
// into AuthNeededError + auth-required health + client reset. Returns nil
// when err is not an OAuth authorization failure.
func (d *Daemon) mapAuthErr(m *managed, cfg config.ServerEntry, err error) error {
	if !cfg.IsRemote() {
		return nil
	}
	if !errors.Is(err, transport.ErrOAuthAuthorizationRequired) &&
		!errors.Is(err, transport.ErrAuthorizationRequired) {
		return nil
	}
	m.setHealth(&Health{LastChecked: time.Now(), AuthRequired: "run: oxyp login " + cfg.Name})
	d.resetClient(m)
	return &AuthNeededError{Server: cfg.Name}
}

// PendingLogin reports whether a browser flow is awaiting manual completion.
// Exposed for tests/CLI; state/verifier stay server-side.
func (d *Daemon) PendingLogin(name string) bool {
	m, err := d.get(name)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending != nil
}

// InvalidateTools drops the discovery cache but keeps the connection.
// Used for tools/list_changed notifications and tests.
func (d *Daemon) InvalidateTools(name string) {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return
	}
	m.toolsMu.Lock()
	m.tools = nil
	m.toolsMu.Unlock()
	if d.cache != nil {
		d.cache.Drop(name)
	}
}

// InvalidateResources drops the resource/prompt/instructions caches but
// keeps the connection. Used for resources/prompts list_changed
// notifications. (Tools have their own disk cache; resources do not.)
func (d *Daemon) InvalidateResources(name string) {
	d.mu.RLock()
	m, ok := d.servers[name]
	d.mu.RUnlock()
	if !ok {
		return
	}
	m.resMu.Lock()
	m.res = nil
	m.prompts = nil
	m.resMu.Unlock()
}

// Instructions returns the server's init-time instructions, capturing them
// on first use (spawns cold servers). Empty = server sent none.
func (d *Daemon) Instructions(ctx context.Context, name string) (string, error) {
	m, err := d.get(name)
	if err != nil {
		return "", err
	}
	m.resMu.RLock()
	if m.instructions != "" {
		s := m.instructions
		m.resMu.RUnlock()
		return s, nil
	}
	m.resMu.RUnlock()
	// clientFor runs initialize, whose result carries instructions; the
	// spawn path caches them. A second check covers the race.
	if _, err := d.clientFor(ctx, m); err != nil {
		d.recordFailure(m, err)
		return "", err
	}
	m.resMu.RLock()
	defer m.resMu.RUnlock()
	return m.instructions, nil
}

// ListResources discovers downstream resources. Order: memory → live
// (spawns cold servers). No disk cache: resource lists are payload-sized
// directory reads, not stable metadata. Same session-expiry retry as tools.
func (d *Daemon) ListResources(ctx context.Context, name string) ([]mcp.Resource, error) {
	m, err := d.get(name)
	if err != nil {
		return nil, err
	}
	m.resMu.RLock()
	if m.res != nil {
		r := m.res
		m.resMu.RUnlock()
		return r, nil
	}
	m.resMu.RUnlock()
	res, err := d.listResourcesOnce(ctx, m)
	cfg := m.getCfg()
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		if cfg.IsRemote() && errors.Is(err, transport.ErrSessionTerminated) {
			d.log.Info("session expired, re-initializing", "server", name)
			d.resetClient(m)
			res, err = d.listResourcesOnce(ctx, m)
		}
	}
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		d.recordFailure(m, err)
		return nil, err
	}
	m.resMu.Lock()
	m.res = res
	m.resMu.Unlock()
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
	return res, nil
}

func (d *Daemon) listResourcesOnce(ctx context.Context, m *managed) ([]mcp.Resource, error) {
	c, err := d.clientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.reqTO)
	defer cancel()
	res, err := c.ListResources(reqCtx, mcp.ListResourcesRequest{})
	if err != nil {
		return nil, err
	}
	return res.Resources, nil
}

// ReadResource reads one downstream resource by URI (passthrough).
// No caching: resource bodies are live data, not discovery metadata.
// URIs containing ".." path segments are rejected before touching
// downstream (cheap traversal guard for query-string probing).
func (d *Daemon) ReadResource(ctx context.Context, name, uri string) (*mcp.ReadResourceResult, error) {
	m, err := d.get(name)
	if err != nil {
		return nil, err
	}
	for _, seg := range strings.Split(uri, "/") {
		if seg == ".." {
			return nil, fmt.Errorf("server %q: refusing resource URI with \"..\": %q", name, uri)
		}
	}
	res, err := d.readResourceOnce(ctx, m, uri)
	cfg := m.getCfg()
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		if cfg.IsRemote() && errors.Is(err, transport.ErrSessionTerminated) {
			d.log.Info("session expired, re-initializing", "server", name)
			d.resetClient(m)
			res, err = d.readResourceOnce(ctx, m, uri)
		}
	}
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		d.recordFailure(m, err)
		return nil, err
	}
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
	return res, nil
}

func (d *Daemon) readResourceOnce(ctx context.Context, m *managed, uri string) (*mcp.ReadResourceResult, error) {
	c, err := d.clientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.reqTO)
	defer cancel()
	return c.ReadResource(reqCtx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: uri},
	})
}

// ListPrompts discovers downstream prompt templates. Order: memory → live
// (spawns cold servers). No disk cache, same session-expiry retry as tools.
func (d *Daemon) ListPrompts(ctx context.Context, name string) ([]mcp.Prompt, error) {
	m, err := d.get(name)
	if err != nil {
		return nil, err
	}
	m.resMu.RLock()
	if m.prompts != nil {
		p := m.prompts
		m.resMu.RUnlock()
		return p, nil
	}
	m.resMu.RUnlock()
	prompts, err := d.listPromptsOnce(ctx, m)
	cfg := m.getCfg()
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		if cfg.IsRemote() && errors.Is(err, transport.ErrSessionTerminated) {
			d.log.Info("session expired, re-initializing", "server", name)
			d.resetClient(m)
			prompts, err = d.listPromptsOnce(ctx, m)
		}
	}
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		d.recordFailure(m, err)
		return nil, err
	}
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
	return prompts, nil
}

func (d *Daemon) listPromptsOnce(ctx context.Context, m *managed) ([]mcp.Prompt, error) {
	c, err := d.clientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.reqTO)
	defer cancel()
	res, err := c.ListPrompts(reqCtx, mcp.ListPromptsRequest{})
	if err != nil {
		return nil, err
	}
	return res.Prompts, nil
}

// childEnv builds the stdio child's environment. inherit_env=false starts
// from a minimal base (PATH, HOME, SYSTEMROOT for Windows tooling) instead
// of the daemon's full env; explicit env overlays apply in both modes.
// PYTHONUNBUFFERED=1 is forced unless already set: MCP-over-stdio is a
// JSONL request/response protocol and CPython block-buffers piped stdout
// by default, which hangs init for servers that don't flush explicitly.
// NOTE: mcp-go's default spawn re-adds os.Environ itself, so clean mode
// only takes effect through commandFuncFor below (which sets cmd.Env
// exactly). Callers must pass both together — see spawn.
func childEnv(cfg config.ServerEntry) []string {
	var env []string
	if cfg.InheritEnv == nil || *cfg.InheritEnv {
		env = os.Environ()
	} else {
		for _, k := range []string{"PATH", "HOME", "SYSTEMROOT"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	hasUnbuffered := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTHONUNBUFFERED=") {
			hasUnbuffered = true
			break
		}
	}
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
		if strings.EqualFold(k, "PYTHONUNBUFFERED") {
			hasUnbuffered = true
		}
	}
	if !hasUnbuffered {
		env = append(env, "PYTHONUNBUFFERED=1")
	}
	return env
}

// commandFuncFor returns a CommandFunc applying cwd and/or clean env, or
// nil for the mcp-go default (daemon's cwd, mcp-go's own env merge).
// A custom func is required whenever Cwd is set OR inherit_env=false:
// without it mcp-go would re-inherit the full daemon env and ignore Cwd.
func commandFuncFor(cfg config.ServerEntry) transport.CommandFunc {
	if cfg.Cwd == "" && (cfg.InheritEnv == nil || *cfg.InheritEnv) {
		return nil
	}
	cwd := cfg.Cwd
	clean := cfg.InheritEnv != nil && !*cfg.InheritEnv
	return func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		if clean {
			cmd.Env = env // exactly: minimal base + overlays, no daemon env
		} else {
			cmd.Env = append(os.Environ(), env...)
		}
		if cwd != "" {
			cmd.Dir = cwd
		}
		return cmd, nil
	}
}

// probeRemote makes one cheap unauthenticated GET against the configured
// URL and turns opaque connect failures into setup hints. It never speaks
// MCP: a 200 with HTML means the URL is a web page, not an MCP endpoint.
// Returns "" when the probe itself fails opaquely (keep the original error).
// Uses the server's TLS config + headers so the hint reflects the real
// connection; URLs are redacted (slice C owns redact.URL).
func probeRemote(cfg config.ServerEntry, timeout time.Duration) string {
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	tlsCfg := remhttp.TransportTLS{}
	if cfg.TLS != nil {
		tlsCfg = remhttp.TransportTLS{
			CAFile: cfg.TLS.CAFile, CertFile: cfg.TLS.CertFile,
			KeyFile: cfg.TLS.KeyFile, Insecure: cfg.TLS.Insecure,
		}
	}
	hc := remhttp.NewClient(timeout, tlsCfg)
	req, err := http.NewRequest(http.MethodGet, cfg.URL, nil)
	if err != nil {
		return ""
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	safeURL := redact.URL(cfg.URL)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return fmt.Sprintf("DNS lookup failed for %q — check the hostname", safeURL)
		}
		var tlsErr *tls.CertificateVerificationError
		if errors.As(err, &tlsErr) {
			return "TLS verification failed — check tls.ca_file or the server certificate"
		}
		msg := err.Error()
		switch {
		case strings.Contains(msg, "connection refused"),
			strings.Contains(msg, "connection reset by peer"):
			return "connection refused — is the server running and is the port right?"
		case strings.Contains(msg, "no such host"):
			return fmt.Sprintf("host not found — check the hostname in %q", safeURL)
		case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
			return "connection timed out — host unreachable or firewall?"
		case strings.Contains(msg, "certificate"):
			return "TLS certificate problem — check tls.ca_file or the server certificate"
		}
		return ""
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(resp.Body, buf)
	sniff := http.DetectContentType(buf[:n])
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "endpoint returned 404 — check the path (is this URL an MCP endpoint?)"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Sprintf("endpoint returned %d — server needs auth (headers/oauth?)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return fmt.Sprintf("endpoint returned %d — is this URL an MCP endpoint?", resp.StatusCode)
	case strings.Contains(sniff, "text/html"):
		return "endpoint returned HTML (200) — this URL does not speak MCP (wrong path?)"
	}
	return ""
}

// CallTool invokes one downstream tool. Spawns cold servers.
// Same session-expiry retry as ListTools.
func (d *Daemon) CallTool(ctx context.Context, name, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	m, err := d.get(name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.inFlight++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.lastUsed = time.Now()
		m.mu.Unlock()
	}()
	res, err := d.callToolOnce(ctx, m, tool, args)
	cfg := m.getCfg()
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		if cfg.IsRemote() && errors.Is(err, transport.ErrSessionTerminated) {
			d.log.Info("session expired, re-initializing", "server", name)
			d.resetClient(m)
			res, err = d.callToolOnce(ctx, m, tool, args)
		}
	}
	if err != nil {
		if aerr := d.mapAuthErr(m, cfg, err); aerr != nil {
			d.recordFailure(m, aerr)
			return nil, aerr
		}
		d.recordFailure(m, err)
		return nil, err
	}
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
	return res, nil
}

func (d *Daemon) callToolOnce(ctx context.Context, m *managed, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	c, err := d.clientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.reqTO)
	defer cancel()
	start := time.Now()
	res, err := c.CallTool(reqCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: tool, Arguments: args},
	})
	cfg := m.getCfg()
	if d.traceFor(m) {
		outcome := "ok"
		blocks := 0
		isErr := false
		if err != nil {
			outcome = "error:" + errClass(err)
		} else if res != nil {
			blocks = len(res.Content)
			isErr = res.IsError
		}
		d.trace().Emit(ptrace.Event{Server: cfg.Name, Direction: "out",
			Method: "tools/call", LatencyMs: time.Since(start).Milliseconds(),
			Outcome: outcome,
			Note:    fmt.Sprintf("tool=%s blocks=%d isError=%v", tool, blocks, isErr)})
	}
	return res, err
}

// recordFailure stores health: auth-needed keeps auth_required (NOT error).
// Error strings are redacted before storage (no secret leak to /health).
func (d *Daemon) recordFailure(m *managed, err error) {
	var authErr *AuthNeededError
	if errors.As(err, &authErr) {
		m.setHealth(&Health{LastChecked: time.Now(), AuthRequired: "run: oxyp login " + authErr.Server})
		return
	}
	m.setHealth(&Health{LastChecked: time.Now(), Error: redact.Body(err.Error())})
}

// Stolen shape from mcpd healthCheckLoop, minus eager connect.
func (d *Daemon) PingLoop(ctx context.Context) {
	t := time.NewTicker(d.healthIv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pingConnected(ctx)
		}
	}
}

func (d *Daemon) pingConnected(ctx context.Context) {
	d.mu.RLock()
	names := make([]string, 0, len(d.servers))
	for n, m := range d.servers {
		m.mu.Lock()
		connected := m.client != nil
		m.mu.Unlock()
		if connected {
			names = append(names, n)
		}
	}
	d.mu.RUnlock()

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			d.mu.RLock()
			m := d.servers[name]
			d.mu.RUnlock()
			m.mu.Lock()
			c := m.client
			m.mu.Unlock()
			if c == nil {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, d.healthTO)
			defer cancel()
			start := time.Now()
			if err := c.Ping(pctx); err != nil {
				m.setHealth(&Health{LastChecked: time.Now(), Error: err.Error()})
				return
			}
			m.setHealth(&Health{Healthy: true, LastChecked: time.Now(), Latency: time.Since(start)})
		}(n)
	}
	wg.Wait()
}

// SweepLoop stops idle stdio children. Local-only: closing a remote HTTP
// handle frees nothing upstream. Runs every 30s; stop is one-at-a-time
// like the adapter (timeout is a minimum, not a deadline).
func (d *Daemon) SweepLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sweepIdle()
		}
	}
}

func (d *Daemon) sweepIdle() {
	dc := d.DaemonConfig()
	d.mu.RLock()
	names := make([]string, 0, len(d.servers))
	for n := range d.servers {
		names = append(names, n)
	}
	d.mu.RUnlock()
	for _, n := range names {
		d.mu.RLock()
		m := d.servers[n]
		d.mu.RUnlock()
		if m == nil {
			continue
		}
		cfg := m.getCfg()
		if cfg.IsRemote() || !cfg.WantsIdleStop(dc) {
			continue
		}
		cutoff := time.Duration(cfg.EffectiveIdleTimeout(dc)) * time.Minute
		m.mu.Lock()
		idle := m.client != nil && m.connecting == nil && m.inFlight == 0 &&
			!m.lastUsed.IsZero() && time.Since(m.lastUsed) >= cutoff
		var c client.MCPClient
		if idle {
			c, m.client = m.client, nil
		}
		lastUsed := m.lastUsed
		m.mu.Unlock()
		if idle {
			d.log.Info("stopping idle server", "server", n,
				"idle", time.Since(lastUsed).Round(time.Second))
			_ = c.Close()
			// keep discovery cache (memory + disk): tools stay
			// searchable, server restarts on next call.
		}
	}
}

// EagerStart connects all eager/keep-alive servers concurrently.
// Failures record health per-server, never fatal (soft-fail rule).
func (d *Daemon) EagerStart(ctx context.Context) {
	d.mu.RLock()
	type eagerTarget struct {
		m    *managed
		name string
	}
	var ms []eagerTarget
	for name, m := range d.servers {
		if cfg := m.getCfg(); cfg.WantsEagerStart() && cfg.ConfigError == "" {
			ms = append(ms, eagerTarget{m: m, name: name})
		}
	}
	d.mu.RUnlock()
	var wg sync.WaitGroup
	for _, t := range ms {
		wg.Add(1)
		go func(t eagerTarget) {
			defer wg.Done()
			if _, err := d.clientFor(ctx, t.m); err != nil {
				d.recordFailure(t.m, err)
				return
			}
			// warm the discovery cache too
			if _, err := d.ListTools(ctx, t.name); err != nil {
				d.recordFailure(t.m, err)
			}
		}(t)
	}
	wg.Wait()
}

// Reload diffs new config against running state: added → tracked cold;
// removed → stopped; modified → restarted cold; unchanged → untouched
// (connections + caches kept). Bad entries become unhealthy, never fatal.
// Modified entries bump the config generation so an in-flight spawn
// discards its stale client (clientFor retries once). Client closes
// happen outside d.mu with the same bounded pattern as Close.
func (d *Daemon) Reload(cfg *config.Config) (added, removed, modified []string) {
	d.mu.Lock()
	d.daemonCfg = cfg.Daemon
	fps := map[string]string{}
	want := map[string]config.ServerEntry{}
	for name, s := range cfg.Servers {
		if s.IsEnabled() {
			s.Name = name
			want[name] = s
		}
	}
	var toClose []client.MCPClient
	closeManaged := func(m *managed) {
		m.mu.Lock()
		c := m.client
		m.client = nil
		m.spawnErr = nil // reload means try again, not serve old failure
		m.mu.Unlock()
		if c != nil {
			toClose = append(toClose, c)
		}
	}
	for name, m := range d.servers {
		ns, ok := want[name]
		if !ok {
			closeManaged(m)
			delete(d.servers, name)
			removed = append(removed, name)
			continue
		}
		m.mu.Lock()
		changed := serverChanged(m.cfg, ns)
		if changed {
			m.cfg = ns
			m.gen++
		}
		m.mu.Unlock()
		if changed {
			closeManaged(m)
			m.toolsMu.Lock()
			m.tools = nil
			m.toolsMu.Unlock()
			m.resMu.Lock()
			m.res = nil
			m.prompts = nil
			m.instructions = ""
			m.resMu.Unlock()
			if ns.ConfigError != "" {
				m.setHealth(&Health{LastChecked: time.Now(), Error: "config: " + ns.ConfigError})
			} else {
				m.setHealth(nil)
			}
			modified = append(modified, name)
		}
		delete(want, name)
	}
	for name, ns := range want {
		m := &managed{cfg: ns}
		if ns.ConfigError != "" {
			m.health = &Health{LastChecked: time.Now(), Error: "config: " + ns.ConfigError}
		}
		d.servers[name] = m
		added = append(added, name)
	}
	for name, m := range d.servers {
		fps[name] = metacache.Fingerprint(m.getCfg())
	}
	if d.cache != nil {
		d.cache.DropStale(fps)
	}
	d.mu.Unlock()
	closeClientsBounded(toClose)
	return added, removed, modified
}

// closeClientsBounded closes clients outside the daemon lock, one bounded
// goroutine each (same pattern as Close).
func closeClientsBounded(cs []client.MCPClient) {
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c client.MCPClient) {
			defer wg.Done()
			done := make(chan struct{})
			go func() { _ = c.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(DefaultShutdownTimeout):
			}
		}(c)
	}
	wg.Wait()
}

// Close stops all children with a bounded timeout each (mcpd pattern).
// It also cancels the daemon-lifetime spawn context, killing any stdio
// children and SSE streams bound to it.
func (d *Daemon) Close() {
	d.mu.RLock()
	ms := make([]*managed, 0, len(d.servers))
	for _, m := range d.servers {
		ms = append(ms, m)
	}
	stop := d.spawnStop
	d.mu.RUnlock()
	if stop != nil {
		stop()
	}
	var cs []client.MCPClient
	for _, m := range ms {
		m.mu.Lock()
		c := m.client
		m.client = nil
		m.mu.Unlock()
		if c != nil {
			cs = append(cs, c)
		}
	}
	closeClientsBounded(cs)
}
func serverChanged(a, b config.ServerEntry) bool {
	// Name is the map key (same on both sides); FromConnector is derived.
	a.Name, b.Name, a.FromConnector, b.FromConnector = "", "", "", ""
	return !reflect.DeepEqual(a, b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
