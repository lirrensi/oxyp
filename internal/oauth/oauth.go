// Package oauth implements daemon-held OAuth 2.1 + PKCE for remote servers.
//
// Policy stolen from lazy-mcp oauth-manager.ts: loopback callback server,
// state-keyed dispatch, file stores at 0600, 60s expiry skew, Bearer
// normalization. Protocol (discovery/DCR/PKCE/refresh) delegated to mcp-go's
// OAuthHandler — strictly stronger than lazy-mcp's SDK wrapper (PRM/RFC 9728,
// issuer binding).
//
// Model (user decision): authorize ONCE at the daemon; tokens live in the
// daemon's dir (~/.oxyp/oauth/); callers never see them. Different identity
// = second daemon. Re-auth at the daemon's box (or SSH-forward the callback).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

// Store is a file-backed transport.TokenStore, one file per server key.
// Files are 0600, dir 0700 (lazy-mcp parity).
type Store struct {
	mu  sync.Mutex
	dir string // <oxypdir>/oauth
	key string // storage key for this server
}

// StorageKey hashes server name + URL so same-name/different-URL entries
// don't collide (lazy-mcp computeStorageKey).
func StorageKey(name, url string) string {
	h := sha256.Sum256([]byte(name + "\x00" + url))
	return hex.EncodeToString(h[:])[:32]
}

// NewStore creates the store, ensuring dir 0700.
func NewStore(dir, key string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, key: key}, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, s.key+".json") }

// GetToken implements transport.TokenStore.
func (s *Store) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path())
	if err != nil {
		return nil, transport.ErrNoToken
	}
	var t transport.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, transport.ErrNoToken // corrupt = missing (lazy-mcp parity)
	}
	return &t, nil
}

// SaveToken implements transport.TokenStore. Atomic write via a UNIQUE temp
// file + rename (a fixed .tmp path races concurrent saves across Store
// instances for the same key).
func (s *Store) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tok-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path())
}

// Delete removes stored tokens (e.g. after confirmed 401).
func (s *Store) Delete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.path())
}

// HasRefreshToken reports whether a refresh token exists.
func (s *Store) HasRefreshToken(ctx context.Context) bool {
	t, err := s.GetToken(ctx)
	return err == nil && t.RefreshToken != ""
}

// --- callback server ---

// CallbackServer is a loopback HTTP server capturing OAuth redirects.
// One per daemon process (lazy-mcp singleton); dispatches by state.
type CallbackServer struct {
	mu       sync.Mutex
	srv      *http.Server
	listener net.Listener
	handlers map[string]chan callbackResult
}

type callbackResult struct {
	code  string
	state string
	err   string
}

// NewCallbackServer binds 127.0.0.1:0 (ephemeral) or a fixed port.
// Fixed port keeps redirect_uri stable across restarts (lazy-mcp :8947);
// ephemeral avoids collisions. Default: fixed 8378, fallback ephemeral.
func NewCallbackServer(port int) (*CallbackServer, error) {
	cs := &CallbackServer{handlers: map[string]chan callbackResult{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cs.handle)
	var ln net.Listener
	var err error
	if port != 0 {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			ln, err = net.Listen("tcp", "127.0.0.1:0")
		}
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	cs.listener = ln
	cs.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = cs.srv.Serve(ln) }()
	return cs, nil
}

// Port returns the bound loopback port.
func (cs *CallbackServer) Port() int {
	_, port, _ := net.SplitHostPort(cs.listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

// RedirectURL returns the callback URL for this server.
func (cs *CallbackServer) RedirectURL() string {
	return fmt.Sprintf("http://localhost:%d/callback", cs.Port())
}

// ExpandRedirectURI renders an oauth.redirect_uri template: "{port}" is
// replaced with the bound callback port. Empty template = default loopback
// URL for that port. A template without {port} is returned verbatim.
func ExpandRedirectURI(template string, port int) string {
	if template == "" {
		return fmt.Sprintf("http://localhost:%d/callback", port)
	}
	return strings.ReplaceAll(template, "{port}", strconv.Itoa(port))
}

// PortFromTemplate parses the fixed port FROM a redirect_uri template that
// lacks {port} (e.g. "http://localhost:8378/callback" → 8378). Empty template
// or one containing {port} has no fixed port: returns 0, nil. Anything else
// unparseable is a login-time error (mismatch with callback_port → error).
func PortFromTemplate(tpl string) (int, error) {
	if tpl == "" || strings.Contains(tpl, "{port}") {
		return 0, nil
	}
	u, err := url.Parse(tpl)
	if err != nil {
		return 0, fmt.Errorf("oauth redirect_uri %q: %w", tpl, err)
	}
	host := u.Host
	if host == "" {
		return 0, fmt.Errorf("oauth redirect_uri %q has no host:port", tpl)
	}
	_, portStr, err := net.SplitHostPort(host)
	if err != nil {
		return 0, fmt.Errorf("oauth redirect_uri %q has no fixed port (add {port} or set callback_port): %v", tpl, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("oauth redirect_uri %q has invalid port %q", tpl, portStr)
	}
	return port, nil
}

// ParseCallbackInput extracts (code, state) from a pasted callback URL or a
// bare code. Accepts full URLs ("http://localhost:PORT/callback?code=..&state=.."),
// query fragments, and bare codes (state returned empty).
func ParseCallbackInput(input string) (code, state string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}
	if !strings.Contains(input, "://") && !strings.Contains(input, "?") &&
		!strings.Contains(input, "=") && !strings.Contains(input, "&") {
		return input, ""
	}
	raw := input
	if !strings.Contains(raw, "://") {
		raw = "http://x/?" + strings.TrimPrefix(raw, "?")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return input, ""
	}
	return u.Query().Get("code"), u.Query().Get("state")
}

// CallbackWait is a registered-but-unawaited callback slot. Register BEFORE
// showURL: an early callback (auto-opened browser, the zoo fake-browser
// pattern) would otherwise hit "unknown oauth state" and hang the login
// until ctx ends. Call Await after showURL; call Cancel to release the slot
// (Await cancels implicitly).
type CallbackWait struct {
	cs    *CallbackServer
	state string
	ch    chan callbackResult
}

// Prepare registers a state and returns its waiter.
func (cs *CallbackServer) Prepare(state string) *CallbackWait {
	ch := make(chan callbackResult, 1)
	cs.mu.Lock()
	cs.handlers[state] = ch
	cs.mu.Unlock()
	return &CallbackWait{cs: cs, state: state, ch: ch}
}

// Cancel releases the slot without waiting.
func (w *CallbackWait) Cancel() {
	w.cs.mu.Lock()
	delete(w.cs.handlers, w.state)
	w.cs.mu.Unlock()
}

// Await blocks until the callback arrives or ctx ends, then releases the slot.
func (w *CallbackWait) Await(ctx context.Context) (code string, err error) {
	defer w.Cancel()
	select {
	case r := <-w.ch:
		if r.err != "" {
			return "", fmt.Errorf("oauth callback: %s", r.err)
		}
		return r.code, nil
	case <-ctx.Done():
		return "", fmt.Errorf("oauth callback: %w", ctx.Err())
	}
}

// Wait registers a state and blocks until the callback arrives or ctx ends.
// Prefer Prepare + showURL + Await (register-before-show); Wait keeps the
// old register-then-block shape for callers that already showed the URL.
func (cs *CallbackServer) Wait(ctx context.Context, state string) (code string, err error) {
	return cs.Prepare(state).Await(ctx)
}

func (cs *CallbackServer) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	cs.mu.Lock()
	ch, ok := cs.handlers[state]
	cs.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("unknown oauth state"))
		return
	}
	var res callbackResult
	if e := q.Get("error"); e != "" {
		res = callbackResult{state: state, err: e + ": " + q.Get("error_description")}
	} else if c := q.Get("code"); c == "" {
		res = callbackResult{state: state, err: "missing code"}
	} else {
		res = callbackResult{state: state, code: c}
	}
	select {
	case ch <- res:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authorized — return to your terminal"))
	default:
		// Duplicate delivery (browser retry, double redirect): the first
		// result is already queued. Never block the handler.
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("callback already received"))
	}
}

// Close shuts the callback server down.
func (cs *CallbackServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = cs.srv.Shutdown(ctx)
}

// --- flow helpers ---

// RandomState returns 128-bit hex state (lazy-mcp parity).
func RandomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CodeVerifier returns a PKCE verifier (43-128 chars).
func CodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CodeChallenge derives the S256 challenge.
func CodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// NormalizeScheme canonicalizes "bearer" → "Bearer" (some servers emit
// lowercase but only accept canonical — lazy-mcp fix).
func NormalizeScheme(scheme string) string {
	if scheme == "" {
		return "Bearer"
	}
	lower := ""
	for _, r := range scheme {
		if r >= 'A' && r <= 'Z' {
			lower += string(r + 32)
		} else {
			lower += string(r)
		}
	}
	if lower == "bearer" {
		return "Bearer"
	}
	return scheme
}
