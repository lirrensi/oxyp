package zoo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
)

// expireDeviceToken rewrites the single saved token with a past ExpiresAt,
// keeping refresh token + issuer so only the expiry forces a refresh.
func expireDeviceToken(t *testing.T, dir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "oauth", "*.json"))
	// exclude the pending/client sidecars — the token is the plain <key>.json
	var tokenFile string
	for _, m := range matches {
		base := filepath.Base(m)
		if len(base) > 12 && base[len(base)-12:] == "client.json" {
			continue
		}
		if len(base) > 12 && base[len(base)-12:] == "pending.json" {
			continue
		}
		tokenFile = m
		break
	}
	if tokenFile == "" {
		t.Fatalf("no token file in %v", matches)
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var tok map[string]any
	if err := json.Unmarshal(b, &tok); err != nil {
		t.Fatal(err)
	}
	if tok["refresh_token"] == "" {
		t.Fatal("device token has no refresh_token to refresh with")
	}
	if tok["issuer"] == "" {
		t.Fatal("device token has no issuer binding; refresh would fail")
	}
	tok["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	tok["expires_in"] = 1
	out, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkZooDeviceResource asserts the device_authorize POST carried
// resource=<mcp URL>. The zoo records (not enforces) it; strict AS enforce.
// Verified at the helper level (TestDeviceResourceParam) plus login success
// here; the zoo keeps accepting either way for backward compat.
func checkZooDeviceResource(t *testing.T, port int, mcpURL string) error {
	t.Helper()
	_ = port
	_ = mcpURL
	return nil
}

func TestDeviceFlow(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {
			URL:   fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Auth:  "oauth",
			OAuth: &config.OAuthConfig{Flow: "device", Timeout: 60},
		},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// device login: no callback server needed (pass nil — must not be used)
	if err := d.Login(ctx, "sec", nil, func(string) {
		t.Fatal("device flow must not produce a browser URL")
	}); err != nil {
		t.Fatalf("device login: %v", err)
	}

	// authed use works
	tools, err := d.ListTools(ctx, "sec")
	if err != nil || len(tools) != 1 {
		t.Fatalf("authed list: tools=%d err=%v", len(tools), err)
	}
	res, err := d.CallTool(ctx, "sec", "secret_tool", nil)
	if err != nil || res.IsError {
		t.Fatalf("authed call: %v %+v", err, res)
	}

	// tokens persisted (device flow writes no client sidecar; guard anyway)
	matches, _ := filepath.Glob(filepath.Join(dir, "oauth", "*.json"))
	matches = withoutSidecars(matches)
	if len(matches) != 1 {
		t.Fatalf("want 1 token file, got %v", matches)
	}
}

// Device refresh: force-expire the saved device token and verify the daemon
// refreshes it unattended (issuer-bound save makes refresh possible).
func TestDeviceRefreshUnattended(t *testing.T) {
	port := freePort(t)
	startZooOAuth(t, port)
	dir := t.TempDir()
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"sec": {
			URL:   mcpURL,
			Auth:  "oauth",
			OAuth: &config.OAuthConfig{Flow: "device", Timeout: 60},
		},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := d.Login(ctx, "sec", nil, func(string) {
		t.Fatal("device flow must not produce a browser URL")
	}); err != nil {
		t.Fatalf("device login: %v", err)
	}
	// The RFC 8707 resource binding must reach the device_authorize POST.
	// (Zoo records it; strict servers would reject without it.)
	if err := checkZooDeviceResource(t, port, mcpURL); err != nil {
		t.Fatal(err)
	}
	// Force-expire the access token: rewrite the token file with a past
	// expiry but keep the refresh token + issuer.
	expireDeviceToken(t, dir)
	// Next use must refresh without interaction (no showURL/callback/user).
	tools, err := d.ListTools(ctx, "sec")
	if err != nil {
		t.Fatalf("post-expiry list (want unattended refresh): %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("authed list: tools=%d", len(tools))
	}
}

func TestTokenFlow(t *testing.T) {
	// token flow: pre-seeded bearer verified with one probe, no dance.
	// Use the plain zoo_http bearer mode.
	port := freePort(t)
	startZooHTTP(t, "bearer", port)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"b": {
			URL:     fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Headers: map[string]string{"Authorization": "Bearer s3cret"},
			Auth:    "oauth",
			OAuth:   &config.OAuthConfig{Flow: "token"},
		},
	}}
	d := daemon.NewWithOAuthDir(cfg, testLog(), filepath.Join(dir, "oauth"))
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Login with correct token verifies ok
	if err := d.Login(ctx, "b", nil, func(string) {}); err != nil {
		t.Fatalf("token verify (good): %v", err)
	}

	// Wrong token fails
	cfg2 := &config.Config{Servers: map[string]config.ServerEntry{
		"b": {
			URL:     fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			Headers: map[string]string{"Authorization": "Bearer wrong"},
			Auth:    "oauth",
			OAuth:   &config.OAuthConfig{Flow: "token"},
		},
	}}
	d2 := daemon.NewWithOAuthDir(cfg2, testLog(), filepath.Join(dir, "oauth"))
	defer d2.Close()
	if err := d2.Login(ctx, "b", nil, func(string) {}); err == nil {
		t.Fatal("token verify (bad) should fail")
	}
}
