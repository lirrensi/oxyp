package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolveCCAuthMethod(t *testing.T) {
	if got := ResolveCCAuthMethod(nil); got != "client_secret_post" {
		t.Fatalf("silent AS: got %q", got)
	}
	if got := ResolveCCAuthMethod([]string{"client_secret_basic", "client_secret_post"}); got != "client_secret_post" {
		t.Fatalf("both offered: got %q", got)
	}
	if got := ResolveCCAuthMethod([]string{"client_secret_basic"}); got != "client_secret_basic" {
		t.Fatalf("basic-only: got %q", got)
	}
	if got := ResolveCCAuthMethod([]string{"none"}); got != "client_secret_post" {
		t.Fatalf("none-only: got %q", got)
	}
}

func TestBasicEncodeRFC6749(t *testing.T) {
	// RFC 6749 §2.3.1: form-encoding. url.QueryEscape gives "+" for space;
	// correct is "%20".
	if got := basicEncode("a b"); got != "a%20b" {
		t.Fatalf("space: got %q", got)
	}
	if got := basicEncode("a+b"); got != "a%2Bb" {
		t.Fatalf("plus: got %q", got)
	}
}

func TestClientRegRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if reg, _ := LoadClientReg(dir, "k"); reg != nil {
		t.Fatal("empty dir must load nil")
	}
	want := ClientReg{ClientID: "id", ClientSecret: "s", AuthMethod: "client_secret_post", Issuer: "https://as/x"}
	if err := SaveClientReg(dir, "k", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClientReg(dir, "k")
	if err != nil || got == nil || *got != want {
		t.Fatalf("round trip: got %+v err %v", got, err)
	}
	// Empty client id persists nothing.
	if err := SaveClientReg(dir, "empty", ClientReg{}); err != nil {
		t.Fatal(err)
	}
	if reg, _ := LoadClientReg(dir, "empty"); reg != nil {
		t.Fatal("empty reg must not persist")
	}
}

func TestPortFromTemplate(t *testing.T) {
	p, err := PortFromTemplate("http://localhost:8378/callback")
	if err != nil || p != 8378 {
		t.Fatalf("fixed: got %d %v", p, err)
	}
	p, err = PortFromTemplate("http://localhost:{port}/callback")
	if err != nil || p != 0 {
		t.Fatalf("templated: got %d %v", p, err)
	}
	p, err = PortFromTemplate("")
	if err != nil || p != 0 {
		t.Fatalf("empty: got %d %v", p, err)
	}
	if _, err := PortFromTemplate("http://localhost/callback"); err == nil {
		t.Fatal("portless must error")
	}
	if _, err := PortFromTemplate("http://localhost:99999/callback"); err == nil {
		t.Fatal("out-of-range port must error")
	}
}

func TestPrepareBeforeShow(t *testing.T) {
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// Register first, then deliver the callback synchronously (what showURL
	// auto-open does). Await must observe it — no "unknown oauth state".
	w := cs.Prepare("s1")
	req, _ := http.NewRequest("GET", "/callback?code=c1&state=s1", nil)
	rec := httptest.NewRecorder()
	cs.handle(rec, req)
	if rec.Code != 200 {
		t.Fatalf("callback for prepared state: status %d", rec.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := w.Await(ctx)
	if err != nil || code != "c1" {
		t.Fatalf("await: code=%q err=%v", code, err)
	}
	// Unregistered state still 400s.
	req2, _ := http.NewRequest("GET", "/callback?code=c&state=nope", nil)
	rec2 := httptest.NewRecorder()
	cs.handle(rec2, req2)
	if rec2.Code != 400 {
		t.Fatalf("unknown state: status %d", rec2.Code)
	}
}

func TestDeviceResourceParam(t *testing.T) {
	var gotAuth, gotPoll string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "device_authorize"):
			gotAuth = string(b)
			_, _ = w.Write([]byte(`{"device_code":"d","user_code":"U","verification_uri":"http://x/d","expires_in":300,"interval":0}`))
		default:
			gotPoll = string(b)
			_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	md := &DeviceMetadata{DeviceAuthorizationEndpoint: srv.URL + "/device_authorize", TokenEndpoint: srv.URL + "/token"}
	dc := &DeviceClient{ClientID: "c"}
	da, err := RequestDeviceCode(ctx, srv.Client(), md, dc, "s", "http://mcp/x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotAuth, "resource=") {
		t.Fatalf("device authorize missing resource: %q", gotAuth)
	}
	if _, _, _, err := PollDeviceToken(ctx, srv.Client(), md, dc, da, time.Minute, "http://mcp/x", func(_, _ string) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPoll, "resource=") {
		t.Fatalf("device poll missing resource: %q", gotPoll)
	}
}

func TestCCResourceAndBasicOnly(t *testing.T) {
	var gotBody string
	var gotAuth string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".well-known/oauth-authorization-server") {
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","token_endpoint":"` + srv.URL + `/token","token_endpoint_auth_methods_supported":["client_secret_basic"]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth = string(b), r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	md, err := DiscoverClientCredentialsMetadata(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if md.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("basic-only AS must select basic, got %q", md.TokenEndpointAuthMethod)
	}
	if _, _, _, err := ClientCredentialsToken(ctx, srv.Client(), md, "i d", "s", "read", "http://mcp/x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "resource=") {
		t.Fatalf("cc token missing resource: %q", gotBody)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("basic-only AS must get Basic auth, got %q", gotAuth)
	}
}
