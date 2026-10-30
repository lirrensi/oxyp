package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"

	"github.com/lirrensi/oxyp/internal/oauth"
	"github.com/lirrensi/oxyp/internal/remhttp"
)

// Login runs the OAuth flow for a server: browser (discovery + DCR via
// mcp-go), device, token-verify, or client_credentials. Any remote without
// explicit auth is eligible (auto-detect, #9); explicit auth="oauth" forces
// it. Client_credentials (grant_type set) takes precedence over flow.
func (d *Daemon) Login(ctx context.Context, name string, cb *oauth.CallbackServer, showURL func(string)) error {
	m, err := d.get(name)
	if err != nil {
		return err
	}
	cfg := m.getCfg()
	if !cfg.IsRemote() || (cfg.Auth != "" && cfg.Auth != "oauth") {
		return fmt.Errorf("server %q is not an oauth remote", name)
	}
	if cfg.OAuth != nil && cfg.OAuth.GrantType == "client_credentials" {
		return d.loginClientCredentials(ctx, m)
	}
	flow := ""
	if cfg.OAuth != nil {
		flow = cfg.OAuth.Flow
	}
	switch flow {
	case "", "browser":
		return d.loginBrowser(ctx, m, cb, showURL)
	case "device":
		return d.loginDevice(ctx, m)
	case "token":
		return d.loginToken(ctx, m)
	default:
		return fmt.Errorf("unknown oauth flow %q", flow)
	}
}

// StartBrowserLogin begins a browser flow: creates the authorize URL and
// returns it with the state+verifier needed to complete it later via
// CompleteBrowserLogin (manual --complete path for loopback-unreachable
// hosts). Caller keeps state/verifier server-side until completion.
func (d *Daemon) StartBrowserLogin(ctx context.Context, name string, cb *oauth.CallbackServer) (authURL, state, verifier string, err error) {
	m, err := d.get(name)
	if err != nil {
		return "", "", "", err
	}
	cfg := m.getCfg()
	if !cfg.IsRemote() || (cfg.Auth != "" && cfg.Auth != "oauth") {
		return "", "", "", fmt.Errorf("server %q is not an oauth remote", name)
	}
	store, err := oauth.NewStore(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL))
	if err != nil {
		return "", "", "", err
	}
	h := d.browserHandler(m, store, cb)
	h.SetBaseURL(cfg.URL)
	// mcp-go v1.4.0 does NOT auto-register: DCR happens only via an explicit
	// RegisterClient call. Without it the handler keeps empty credentials and
	// strict IdPs reject the exchange/refresh with invalid_client. Static
	// client_id flows skip this; failures here are login-time errors.
	if cfg.OAuth == nil || cfg.OAuth.ClientID == "" {
		if err := h.RegisterClient(ctx, "oxyp"); err != nil {
			return "", "", "", fmt.Errorf("oauth client registration: %w", err)
		}
	}
	state, err = oauth.RandomState()
	if err != nil {
		return "", "", "", err
	}
	verifier, err = oauth.CodeVerifier()
	if err != nil {
		return "", "", "", err
	}
	authURL, err = h.GetAuthorizationURL(ctx, state, oauth.CodeChallenge(verifier))
	if err != nil {
		return "", "", "", fmt.Errorf("oauth authorize url: %w", err)
	}
	// Persist the DCR credentials alongside the pending flow: a cross-process
	// --complete rebuilds the handler from scratch and must present the same
	// client_id a strict IdP binds the code to. Additive sidecar — no token
	// file migration.
	key := oauth.StorageKey(cfg.Name, cfg.URL)
	_ = oauth.SaveClientReg(d.oauthDir, key, oauth.SnapshotClientReg(h))
	// Keep the pending flow server-side so CompleteBrowserLogin can reuse
	// the exact transaction (issuer/endpoints/credentials/DCR).
	txn, _ := h.GetAuthorizationTransaction()
	m.mu.Lock()
	m.pending = &pendingLogin{handler: h, state: state, verifier: verifier, txn: txn}
	m.mu.Unlock()
	return authURL, state, verifier, nil
}

// CompleteBrowserLogin finishes a browser flow started by StartBrowserLogin:
// exchanges code for tokens. input is the pasted callback URL or bare code.
// Same-process: reuses the pending handler+transaction (state/verifier
// optional). Cross-process/CLI: pass the saved state+verifier; the handler
// is rebuilt and the transaction restored.
func (d *Daemon) CompleteBrowserLogin(ctx context.Context, name, input, state, verifier string, h *transport.OAuthHandler) error {
	m, err := d.get(name)
	if err != nil {
		return err
	}
	cfg := m.getCfg()
	code, cbState := oauth.ParseCallbackInput(input)
	if code == "" {
		return fmt.Errorf("no code found in input")
	}
	if cbState != "" {
		state = cbState
	}
	// Same-process fast path: pending handler carries the exact transaction.
	m.mu.Lock()
	pend := m.pending
	m.mu.Unlock()
	if h == nil && pend != nil && (state == "" || state == pend.state) {
		h, state, verifier = pend.handler, pend.state, pend.verifier
		if err := h.SetAuthorizationTransaction(pend.txn); err != nil {
			return fmt.Errorf("oauth restore flow: %w", err)
		}
		if err := h.ProcessAuthorizationCallback(ctx, url.Values{"code": {code}, "state": {state}}, verifier); err != nil {
			return fmt.Errorf("oauth exchange: %w", err)
		}
		// Tokens saved by the handler; refresh credentials survive restarts
		// via the sidecar written at Start/loginBrowser time.
		m.mu.Lock()
		m.pending = nil
		m.mu.Unlock()
		d.resetClient(m)
		return nil
	}
	if h == nil {
		key := oauth.StorageKey(cfg.Name, cfg.URL)
		saved, _ := oauth.LoadPending(ctx, d.oauthDir, key)
		if state == "" || verifier == "" {
			// Cross-process (CLI --complete): load the saved pending flow.
			if saved != nil {
				state, verifier = saved.State, saved.Verifier
			}
		}
		if state == "" || verifier == "" {
			return fmt.Errorf("no pending login for %q (run oxyp login %s first, then paste the callback URL)", name, name)
		}
		defer oauth.ClearPending(d.oauthDir, key)
		// Same redirect URI must be reused so DCR credentials match.
		// Prefer the saved URI (exact bound port); else the literal
		// template (no {port}); a {port} template needs the original port.
		redirectURI := OAuthCallbackURL(cfg)
		if saved != nil && saved.RedirectURI != "" {
			redirectURI = saved.RedirectURI
		} else if cfg.OAuth != nil && cfg.OAuth.RedirectURI != "" &&
			!strings.Contains(cfg.OAuth.RedirectURI, "{port}") {
			redirectURI = cfg.OAuth.RedirectURI
		}
		store, serr := oauth.NewStore(d.oauthDir, key)
		if serr != nil {
			return serr
		}
		ocfg := transport.OAuthConfig{RedirectURI: redirectURI, TokenStore: store, PKCEEnabled: true}
		if cfg.OAuth != nil {
			ocfg.ClientID = cfg.OAuth.ClientID
			ocfg.ClientSecret = cfg.OAuth.ClientSecret
			if cfg.OAuth.Scope != "" {
				ocfg.Scopes = []string{cfg.OAuth.Scope}
			}
		}
		// Cross-process DCR recovery: the pending flow carries the creds
		// when Start ran in-process; the sidecar covers loginBrowser's
		// out-of-band approval. Configured values always win.
		if saved != nil {
			oauth.ApplyClientReg(&ocfg, &oauth.ClientReg{
				ClientID: saved.ClientID, ClientSecret: saved.ClientSecret,
				AuthMethod: saved.AuthMethod, Issuer: saved.Issuer,
			})
		}
		if reg, _ := oauth.LoadClientReg(d.oauthDir, key); reg != nil {
			oauth.ApplyClientReg(&ocfg, reg)
		}
		h = transport.NewOAuthHandler(ocfg)
		h.SetBaseURL(cfg.URL)
		h.SetExpectedState(state)
	}
	if err := h.ProcessAuthorizationResponse(ctx, code, state, verifier); err != nil {
		return fmt.Errorf("oauth exchange: %w", err)
	}
	m.mu.Lock()
	m.pending = nil
	m.mu.Unlock()
	d.resetClient(m)
	return nil
}

// browserHandler builds the mcp-go handler for a browser flow, resolving the
// redirect URI: oauth.redirect_uri template ({port} → bound port) wins;
// otherwise the bound callback URL; nil cb (manual complete, no loopback)
// falls back to OAuthCallbackURL (fixed :8378 or callback_port).
func (d *Daemon) browserHandler(m *managed, store *oauth.Store, cb *oauth.CallbackServer) *transport.OAuthHandler {
	cfg := m.getCfg()
	redirectURI := OAuthCallbackURL(cfg)
	if cb != nil {
		tmpl := ""
		if cfg.OAuth != nil {
			tmpl = cfg.OAuth.RedirectURI
		}
		redirectURI = oauth.ExpandRedirectURI(tmpl, cb.Port())
	} else if cfg.OAuth != nil && cfg.OAuth.RedirectURI != "" && !strings.Contains(cfg.OAuth.RedirectURI, "{port}") {
		redirectURI = cfg.OAuth.RedirectURI
	}
	ocfg := transport.OAuthConfig{
		RedirectURI: redirectURI,
		TokenStore:  store,
		PKCEEnabled: true,
	}
	if cfg.OAuth != nil {
		ocfg.ClientID = cfg.OAuth.ClientID
		ocfg.ClientSecret = cfg.OAuth.ClientSecret
		if cfg.OAuth.Scope != "" {
			ocfg.Scopes = []string{cfg.OAuth.Scope}
		}
	}
	// Restart recovery: backfill DCR credentials from the sidecar so refresh
	// after a restart presents the registered client_id. Slice A calls this
	// from remoteStack too; both paths share the helper. Configured values win.
	if key := oauth.StorageKey(cfg.Name, cfg.URL); ocfg.ClientID == "" {
		if reg, _ := oauth.LoadClientReg(d.oauthDir, key); reg != nil {
			oauth.ApplyClientReg(&ocfg, reg)
		}
	}
	return transport.NewOAuthHandler(ocfg)
}

func (d *Daemon) loginBrowser(ctx context.Context, m *managed, cb *oauth.CallbackServer, showURL func(string)) error {
	if d.oauthDir == "" {
		return fmt.Errorf("oauth store unavailable (no oauth dir)")
	}
	if cb == nil {
		return fmt.Errorf("browser flow needs a callback server (or complete a pending flow with --complete)")
	}
	store, err := oauth.NewStore(d.oauthDir, oauth.StorageKey(m.getCfg().Name, m.getCfg().URL))
	if err != nil {
		return err
	}
	cfg := m.getCfg()
	h := d.browserHandler(m, store, cb)
	h.SetBaseURL(cfg.URL)

	// See StartBrowserLogin: mcp-go needs an explicit RegisterClient for DCR.
	if cfg.OAuth == nil || cfg.OAuth.ClientID == "" {
		if err := h.RegisterClient(ctx, "oxyp"); err != nil {
			return fmt.Errorf("oauth client registration: %w", err)
		}
	}
	state, err := oauth.RandomState()
	if err != nil {
		return err
	}
	verifier, err := oauth.CodeVerifier()
	if err != nil {
		return err
	}
	authURL, err := h.GetAuthorizationURL(ctx, state, oauth.CodeChallenge(verifier))
	if err != nil {
		return fmt.Errorf("oauth authorize url: %w", err)
	}
	// Persist the pending flow (with post-DCR creds) so --complete works
	// cross-process when the loopback isn't reachable (SSH case). The sidecar
	// covers the same creds for the restart-refresh path.
	key := oauth.StorageKey(cfg.Name, cfg.URL)
	tmpl := ""
	if cfg.OAuth != nil {
		tmpl = cfg.OAuth.RedirectURI
	}
	reg := oauth.SnapshotClientReg(h)
	_ = oauth.SaveClientReg(d.oauthDir, key, reg)
	_ = oauth.SavePending(d.oauthDir, key, oauth.PendingFlow{
		State: state, Verifier: verifier,
		RedirectURI: oauth.ExpandRedirectURI(tmpl, cb.Port()),
		ClientID:    reg.ClientID, ClientSecret: reg.ClientSecret,
		AuthMethod: reg.AuthMethod, Issuer: reg.Issuer,
	})
	txn, _ := h.GetAuthorizationTransaction()
	m.mu.Lock()
	m.pending = &pendingLogin{handler: h, state: state, verifier: verifier, txn: txn}
	m.mu.Unlock()
	// Register BEFORE showURL: an auto-opened browser (or the zoo
	// fake-browser) can callback before Wait would run; unregistered states
	// get 400 "unknown oauth state" and the login hangs to ctx end.
	waiter := cb.Prepare(state)
	showURL(authURL)

	code, err := waiter.Await(ctx)
	if err != nil {
		return err
	}
	if err := h.ProcessAuthorizationResponse(ctx, code, state, verifier); err != nil {
		return fmt.Errorf("oauth exchange: %w", err)
	}
	oauth.ClearPending(d.oauthDir, key)
	m.mu.Lock()
	m.pending = nil
	m.mu.Unlock()
	// Fresh tokens: drop any cached client so next use picks them up.
	d.resetClient(m)
	return nil
}

func (d *Daemon) loginDevice(ctx context.Context, m *managed) error {
	if d.oauthDir == "" {
		return fmt.Errorf("oauth store unavailable (no oauth dir)")
	}
	cfg := m.getCfg()
	httpClient := remhttpClient(d, m)
	issuer := issuerOf(cfg.URL)
	md, err := oauth.DiscoverDeviceMetadata(ctx, httpClient, issuer)
	if err != nil {
		return err
	}
	var clientID, clientSecret, scope string
	timeout := 300 * time.Second
	if cfg.OAuth != nil {
		clientID, clientSecret = cfg.OAuth.ClientID, cfg.OAuth.ClientSecret
		scope = cfg.OAuth.Scope
		if cfg.OAuth.Timeout > 0 {
			timeout = time.Duration(cfg.OAuth.Timeout) * time.Second
		}
	}
	dc, err := oauth.RegisterDeviceClient(ctx, httpClient, md, clientID, clientSecret)
	if err != nil {
		return err
	}
	// RFC 8707: bind device authorize + token polls to the MCP endpoint so
	// resource-enforcing AS accept the flow.
	da, err := oauth.RequestDeviceCode(ctx, httpClient, md, dc, scope, cfg.URL)
	if err != nil {
		return err
	}
	access, refresh, expiresIn, err := oauth.PollDeviceToken(ctx, httpClient, md, dc, da, timeout, cfg.URL,
		func(uri, code string) {
			fmt.Fprintf(os.Stderr, "\noxyp oauth: open %s on any device.\nCode: %s\nWaiting for approval...\n\n", uri, code)
		})
	if err != nil {
		return err
	}
	store, err := oauth.NewStore(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL))
	if err != nil {
		return err
	}
	// Issuer binding is REQUIRED: mcp-go's refreshToken rejects an empty
	// issuer outright, so an unbound device token could never refresh
	// unattended. md.Issuer is the discovered AS identity (same bound-save
	// shape the browser flow gets via saveBoundToken).
	tok := &transport.Token{
		Issuer:      md.Issuer,
		AccessToken: access, RefreshToken: refresh, TokenType: "Bearer",
		ExpiresIn: expiresIn,
	}
	if expiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	if err := store.SaveToken(ctx, tok); err != nil {
		return err
	}
	d.resetClient(m)
	return nil
}

// loginClientCredentials runs grant_type=client_credentials: non-interactive
// token POST with client_id/secret (+scope), no browser/callback. Refresh
// afterwards flows through the mcp-go handler (stored refresh token).
func (d *Daemon) loginClientCredentials(ctx context.Context, m *managed) error {
	if d.oauthDir == "" {
		return fmt.Errorf("oauth store unavailable (no oauth dir)")
	}
	cfg := m.getCfg()
	var clientID, clientSecret, scope string
	if cfg.OAuth != nil {
		clientID, clientSecret, scope = cfg.OAuth.ClientID, cfg.OAuth.ClientSecret, cfg.OAuth.Scope
	}
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("client_credentials needs oauth.client_id and oauth.client_secret")
	}
	httpClient := remhttpClient(d, m)
	md, err := oauth.DiscoverClientCredentialsMetadata(ctx, httpClient, issuerOf(cfg.URL))
	if err != nil {
		return err
	}
	// RFC 8707: bind the token POST to the MCP endpoint so
	// resource-enforcing AS accept the flow.
	access, refresh, expiresIn, err := oauth.ClientCredentialsToken(ctx, httpClient, md, clientID, clientSecret, scope, cfg.URL)
	if err != nil {
		return err
	}
	store, err := oauth.NewStore(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL))
	if err != nil {
		return err
	}
	if err := oauth.SaveClientCredentialsToken(ctx, store, md.Issuer, access, refresh, expiresIn); err != nil {
		return err
	}
	d.resetClient(m)
	return nil
}

func (d *Daemon) loginToken(ctx context.Context, m *managed) error {
	cfg := m.getCfg()
	// token flow: no dance — verify the static header works with one probe.
	if _, ok := cfg.Headers["Authorization"]; !ok {
		for k := range cfg.Headers {
			if strings.EqualFold(k, "authorization") {
				goto hasAuth
			}
		}
		return fmt.Errorf("token flow needs headers.Authorization set")
	}
hasAuth:
	d.resetClient(m)
	c, err := d.clientFor(ctx, m)
	if err != nil {
		return fmt.Errorf("token verify failed: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.reqTO)
	defer cancel()
	if err := c.Ping(reqCtx); err != nil {
		// ping unsupported? fall back to tools/list
		if _, lerr := d.ListTools(ctx, cfg.Name); lerr != nil {
			return fmt.Errorf("token verify failed: %w", err)
		}
	}
	return nil
}

// remhttpClient builds the hardened HTTP client for OAuth side quests
// (device discovery/DCR/poll use plain HTTP, not the MCP transport).
func remhttpClient(d *Daemon, m *managed) *http.Client {
	cfg := m.getCfg()
	tlsCfg := remhttp.TransportTLS{}
	if cfg.TLS != nil {
		tlsCfg = remhttp.TransportTLS{
			CAFile: cfg.TLS.CAFile, CertFile: cfg.TLS.CertFile,
			KeyFile: cfg.TLS.KeyFile, Insecure: cfg.TLS.Insecure,
		}
	}
	return remhttp.NewClient(d.reqTO, tlsCfg)
}

// issuerOf derives the OAuth issuer base (scheme://host[:port]) from the MCP URL.
func issuerOf(mcpURL string) string {
	if u, err := url.Parse(mcpURL); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return strings.TrimSuffix(mcpURL, "/mcp")
}
func (d *Daemon) AuthURLFor(ctx context.Context, name string) (string, error) {
	m, err := d.get(name)
	if err != nil {
		return "", err
	}
	cfg := m.getCfg()
	if !cfg.IsRemote() || (cfg.Auth != "" && cfg.Auth != "oauth") || d.oauthDir == "" {
		return "", fmt.Errorf("server %q is not an oauth remote", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store, err := oauth.NewStore(d.oauthDir, oauth.StorageKey(cfg.Name, cfg.URL))
	if err != nil {
		return "", err
	}
	h := d.browserHandler(m, store, nil)
	h.SetBaseURL(cfg.URL)
	state, _ := oauth.RandomState()
	verifier, _ := oauth.CodeVerifier()
	return h.GetAuthorizationURL(ctx, state, oauth.CodeChallenge(verifier))
}
