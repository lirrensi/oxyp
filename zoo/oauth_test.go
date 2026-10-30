package zoo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"oxyp/internal/config"
	"oxyp/internal/daemon"
	"oxyp/internal/oauth"
)

func startZooOAuth(t *testing.T, port int) {
	t.Helper()
	cmd := exec.Command("python3", filepath.Join(zooPath(t), "http", "zoo_oauth.py"),
		"--port", fmt.Sprint(port))
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for range 50 {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/.well-known/oauth-authorization-server", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("zoo-oauth never listened on %d", port)
}

// fakeBrowser acts as the user: GETs the authorize URL (no redirect follow),
// then GETs the Location (the loopback callback). Returns error (never
// t.Fatal — it runs off the test goroutine).
func fakeBrowser(authURL string) error {
	noRedir := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedir.Get(authURL)
	if err != nil {
		return fmt.Errorf("browser authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 && resp.StatusCode != 303 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("browser authorize: status=%d body=%s", resp.StatusCode, b)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return fmt.Errorf("browser authorize: no Location")
	}
	resp2, err := http.Get(loc)
	if err != nil {
		return fmt.Errorf("browser callback: %w", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return fmt.Errorf("browser callback: status=%d", resp2.StatusCode)
	}
	return nil
}

func TestOAuthFlow(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), Auth: "oauth"},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. unauthenticated use → AuthNeededError + auth_required health
	_, err := d.ListTools(ctx, "sec")
	if _, ok := err.(*daemon.AuthNeededError); !ok {
		t.Fatalf("want AuthNeededError, got %v", err)
	}
	h := d.HealthOf("sec")
	if h == nil || h.AuthRequired == "" || h.Error != "" {
		t.Fatalf("want auth_required without error, got %+v", h)
	}

	// 2. login with fake browser
	cb, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	loginErr := make(chan error, 1)
	go func() {
		loginErr <- d.Login(ctx, "sec", cb, func(url string) {
			go func() {
				if err := fakeBrowser(url); err != nil {
					t.Error(err)
				}
			}()
		})
	}()
	select {
	case err := <-loginErr:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("login timed out")
	}

	// 3. authenticated use works
	tools, err := d.ListTools(ctx, "sec")
	if err != nil || len(tools) != 1 || tools[0].Name != "secret_tool" {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
	res, err := d.CallTool(ctx, "sec", "secret_tool", nil)
	if err != nil || res.IsError {
		t.Fatalf("authed call: %v %+v", err, res)
	}

	// 4. tokens persisted 0600 (sidecars excluded from the count)
	matches, _ := filepath.Glob(filepath.Join(dir, "oauth", "*.json"))
	matches = withoutSidecars(matches)
	if len(matches) != 1 {
		t.Fatalf("want 1 token file, got %v", matches)
	}
}

// Ephemeral port + {port} template: login binds :0, advertises the actual
// port in redirect_uri, and the flow completes.
func TestOAuthEphemeralPortTemplate(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {
			URL:  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Auth: "oauth",
			OAuth: &config.OAuthConfig{
				RedirectURI: "http://localhost:{port}/callback",
			},
		},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cb, err := oauth.NewCallbackServer(0) // ephemeral
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if cb.Port() == 0 || cb.Port() == 8378 {
		t.Fatalf("want OS-assigned ephemeral port, got %d", cb.Port())
	}
	wantRedirect := fmt.Sprintf("http://localhost:%d/callback", cb.Port())
	if got := oauth.ExpandRedirectURI(cfg.Servers["sec"].OAuth.RedirectURI, cb.Port()); got != wantRedirect {
		t.Fatalf("template expand: got %q want %q", got, wantRedirect)
	}
	loginErr := make(chan error, 1)
	go func() {
		loginErr <- d.Login(ctx, "sec", cb, func(url string) {
			if !containsPort(url, cb.Port()) {
				t.Errorf("authorize url missing ephemeral port: %s", url)
			}
			go func() {
				if err := fakeBrowser(url); err != nil {
					t.Error(err)
				}
			}()
		})
	}()
	select {
	case err := <-loginErr:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("login timed out")
	}
	tools, err := d.ListTools(ctx, "sec")
	if err != nil || len(tools) != 1 {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
}

func containsPort(authURL string, port int) bool {
	return contains(authURL, fmt.Sprint(port))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

// Manual complete: start a browser flow, then finish it with the pasted
// callback URL (no loopback reachability needed at complete time).
func TestOAuthManualComplete(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), Auth: "oauth"},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cb, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	authURL, _, _, err := d.StartBrowserLogin(ctx, "sec", cb)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !d.PendingLogin("sec") {
		t.Fatal("want pending login after start")
	}
	// fakeBrowser without following the loopback redirect: capture Location.
	noRedir := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	resp, err := noRedir.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("no Location in authorize response")
	}
	if err := d.CompleteBrowserLogin(ctx, "sec", loc, "", "", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if d.PendingLogin("sec") {
		t.Fatal("pending login should clear after complete")
	}
	tools, err := d.ListTools(ctx, "sec")
	if err != nil || len(tools) != 1 {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
}

// Manual complete across processes: the completing daemon has no in-memory
// pending flow; it loads state/verifier/redirect_uri from the oauth dir.
func TestOAuthManualCompleteCrossProcess(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	oauthDir := filepath.Join(dir, "oauth")
	mkCfg := func() *config.Config {
		return &config.Config{Servers: map[string]config.ServerEntry{
			"sec": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), Auth: "oauth"},
		}}
	}
	d1 := daemon.NewWithOAuthDir(mkCfg(), testLog(), oauthDir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cb, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	// loginBrowser persists the pending flow server-side.
	loginErr := make(chan error, 1)
	go func() {
		loginErr <- d1.Login(ctx, "sec", cb, func(authURL string) {
			// Approve out-of-band: capture the redirect without touching
			// the loopback, then kill the first login's wait.
			noRedir := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				Timeout:       10 * time.Second,
			}
			resp, err := noRedir.Get(authURL)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			loc := resp.Header.Get("Location")
			if loc == "" {
				t.Error("no Location in authorize response")
				return
			}
			// Fresh daemon, same oauth dir: no in-memory pending flow.
			d2 := daemon.NewWithOAuthDir(mkCfg(), testLog(), oauthDir)
			defer d2.Close()
			if err := d2.CompleteBrowserLogin(ctx, "sec", loc, "", "", nil); err != nil {
				t.Errorf("cross-process complete: %v", err)
				return
			}
			tools, err := d2.ListTools(ctx, "sec")
			if err != nil || len(tools) != 1 {
				t.Errorf("authed list: tools=%d err=%v", len(tools), err)
			}
		})
	}()
	// Give the out-of-band approval a head start, then cancel the waiter.
	time.Sleep(2 * time.Second)
	cancel()
	_ = <-loginErr
	d1.Close()
}
func TestOAuthClientCredentials(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"svc": {
			URL:  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Auth: "oauth",
			OAuth: &config.OAuthConfig{
				GrantType:    "client_credentials",
				ClientID:     "zoo-svc",
				ClientSecret: "zoo-svc-secret",
				Scope:        "read",
			},
		},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// No callback server, showURL must never fire.
	if err := d.Login(ctx, "svc", nil, func(string) {
		t.Fatal("client_credentials must not produce a browser URL")
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	tools, err := d.ListTools(ctx, "svc")
	if err != nil || len(tools) != 1 {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "oauth", "*.json"))
	matches = withoutSidecars(matches)
	if len(matches) != 1 {
		t.Fatalf("want 1 token file, got %v", matches)
	}

	// Wrong secret fails.
	cfgBad := &config.Config{Servers: map[string]config.ServerEntry{
		"svc": {
			URL:  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Auth: "oauth",
			OAuth: &config.OAuthConfig{
				GrantType:    "client_credentials",
				ClientID:     "zoo-svc",
				ClientSecret: "wrong",
			},
		},
	}}
	dBad := daemon.NewWithOAuthDir(cfgBad, testLog(), filepath.Join(dir, "oauth-bad"))
	defer dBad.Close()
	if err := dBad.Login(ctx, "svc", nil, func(string) {}); err == nil {
		t.Fatal("bad secret should fail")
	}
}

// Auto-detect: a remote with NO auth key but 401+resource_metadata yields
// auth_required (not error) and login succeeds.
func TestOAuthAutoDetect(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port)},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := d.ListTools(ctx, "sec")
	if _, ok := err.(*daemon.AuthNeededError); !ok {
		t.Fatalf("want AuthNeededError, got %v", err)
	}
	h := d.HealthOf("sec")
	if h == nil || h.AuthRequired == "" || h.Error != "" {
		t.Fatalf("want auth_required without error, got %+v", h)
	}

	cb, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	loginErr := make(chan error, 1)
	go func() {
		loginErr <- d.Login(ctx, "sec", cb, func(url string) {
			go func() {
				if err := fakeBrowser(url); err != nil {
					t.Error(err)
				}
			}()
		})
	}()
	select {
	case err := <-loginErr:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("login timed out")
	}
	tools, err := d.ListTools(ctx, "sec")
	if err != nil || len(tools) != 1 {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
}

// DCR sidecar: after a browser login, the registration (client_id issued by
// the zoo /register) is persisted next to the token; a fresh daemon on the
// same oauth dir picks it up, and a cross-process --complete against the
// strict client_id zoo succeeds.
func TestOAuthDCRSidecarRestart(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	oauthDir := filepath.Join(dir, "oauth")
	mkCfg := func() *config.Config {
		return &config.Config{Servers: map[string]config.ServerEntry{
			"sec": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), Auth: "oauth"},
		}}
	}
	d1 := daemon.NewWithOAuthDir(mkCfg(), testLog(), oauthDir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cb, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	loginErr := make(chan error, 1)
	go func() {
		loginErr <- d1.Login(ctx, "sec", cb, func(url string) {
			go func() {
				if err := fakeBrowser(url); err != nil {
					t.Error(err)
				}
			}()
		})
	}()
	select {
	case err := <-loginErr:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("login timed out")
	}
	d1.Close()
	// Sidecar exists with the zoo-issued client id.
	matches, _ := filepath.Glob(filepath.Join(oauthDir, "*.client.json"))
	if len(matches) != 1 {
		t.Fatalf("want 1 client sidecar, got %v", matches)
	}
	reg, err := oauth.LoadClientReg(oauthDir, sidecarKey(t, oauthDir))
	if err != nil || reg == nil {
		t.Fatalf("LoadClientReg: reg=%+v err=%v", reg, err)
	}
	if reg.ClientID != "zoo-client-1" {
		t.Fatalf("want zoo-client-1, got %q", reg.ClientID)
	}
	if reg.Issuer == "" {
		t.Fatal("sidecar must carry the credentials issuer for refresh binding")
	}
	// Fresh daemon, same dir: the CLI-equivalent path (loginBrowser saves
	// pending + sidecar; out-of-band approval; another process completes).
	d2 := daemon.NewWithOAuthDir(mkCfg(), testLog(), oauthDir)
	cb2, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb2.Close()
	loginErr2 := make(chan error, 1)
	var locMu sync.Mutex
	var loc string
	go func() {
		loginErr2 <- d2.Login(ctx, "sec", cb2, func(authURL string) {
			noRedir := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				Timeout:       10 * time.Second,
			}
			resp, err := noRedir.Get(authURL)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			locMu.Lock()
			loc = resp.Header.Get("Location")
			locMu.Unlock()
		})
	}()
	// Wait for the out-of-band approval to land, then kill the waiter and
	// complete from a third process with no in-memory state.
	deadline := time.Now().Add(30 * time.Second)
	for {
		locMu.Lock()
		got := loc
		locMu.Unlock()
		if got != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("out-of-band approval never landed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	d3 := daemon.NewWithOAuthDir(mkCfg(), testLog(), oauthDir)
	defer d3.Close()
	locMu.Lock()
	locCopy := loc
	locMu.Unlock()
	if err := d3.CompleteBrowserLogin(ctx, "sec", locCopy, "", "", nil); err != nil {
		t.Fatalf("cross-process complete with sidecar: %v", err)
	}
	tools, err := d3.ListTools(ctx, "sec")
	if err != nil || len(tools) != 1 {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
	cancel()
	_ = <-loginErr2
	d2.Close()
}

// sidecarKey finds the storage key by locating the single client sidecar.
func sidecarKey(t *testing.T, oauthDir string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(oauthDir, "*.client.json"))
	if len(matches) != 1 {
		t.Fatalf("want 1 client sidecar, got %v", matches)
	}
	base := filepath.Base(matches[0])
	return base[:len(base)-len(".client.json")]
}

// Fixed redirect_uri template without {port}: the CLI-equivalent path binds
// the template's port; a callback_port mismatch is a login-time error.
func TestOAuthFixedTemplatePort(t *testing.T) {
	if got, err := oauth.PortFromTemplate("http://localhost:8378/callback"); err != nil || got != 8378 {
		t.Fatalf("fixed template: got %d err %v", got, err)
	}
	if got, err := oauth.PortFromTemplate("http://localhost:{port}/callback"); err != nil || got != 0 {
		t.Fatalf("{port} template: got %d err %v", got, err)
	}
	if got, err := oauth.PortFromTemplate(""); err != nil || got != 0 {
		t.Fatalf("empty template: got %d err %v", got, err)
	}
	if _, err := oauth.PortFromTemplate("http://localhost/callback"); err == nil {
		t.Fatal("portless template must error")
	}
	// End-to-end: fixed template binds its port and the flow completes.
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	fixedCb := freePort(t)
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {
			URL:  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Auth: "oauth",
			OAuth: &config.OAuthConfig{
				RedirectURI: fmt.Sprintf("http://localhost:%d/callback", fixedCb),
			},
		},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tplPort, err := oauth.PortFromTemplate(cfg.Servers["sec"].OAuth.RedirectURI)
	if err != nil || tplPort != fixedCb {
		t.Fatalf("template port: got %d err %v", tplPort, err)
	}
	cb, err := oauth.NewCallbackServer(tplPort)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if cb.Port() != fixedCb {
		t.Fatalf("callback bound %d, want fixed %d", cb.Port(), fixedCb)
	}
	loginErr := make(chan error, 1)
	go func() {
		loginErr <- d.Login(ctx, "sec", cb, func(url string) {
			go func() {
				if err := fakeBrowser(url); err != nil {
					t.Error(err)
				}
			}()
		})
	}()
	select {
	case err := <-loginErr:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("login timed out")
	}
}

// Callback race: showURL fires a synchronous callback (register-before-show);
// login must not hang on "unknown oauth state".
func TestOAuthCallbackRace(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), Auth: "oauth"},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cb, err := oauth.NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	// Synchronous fake browser: callback lands inside showURL, before any
	// Wait-style await could run. Register-before-show handles it.
	done := make(chan error, 1)
	go func() {
		done <- d.Login(ctx, "sec", cb, func(url string) {
			if err := fakeBrowser(url); err != nil {
				t.Error(err)
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("login: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("login hung: callback race (state not registered before showURL)")
	}
}
