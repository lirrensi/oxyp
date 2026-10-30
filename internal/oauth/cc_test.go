package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDiscoverCCMetadata(t *testing.T) {
	// Trailing slash issuer.
	meta := map[string]any{"issuer": "https://iss", "token_endpoint": "https://iss/token"}
	raw, _ := json.Marshal(meta)
	hit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			t.Errorf("path: %s", r.URL.Path)
		}
		hit++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	md, err := DiscoverClientCredentialsMetadata(context.Background(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if hit != 1 || md.TokenEndpoint != "https://iss/token" {
		t.Fatalf("discover: %+v", md)
	}
	// 500, bad JSON, no token endpoint.
	for name, tc := range map[string]struct {
		code int
		body string
	}{
		"status-500":  {500, `x`},
		"bad-json":    {200, `{bad`},
		"no-endpoint": {200, `{"issuer":"i"}`},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		}))
		func() {
			defer s.Close()
			if _, err := DiscoverClientCredentialsMetadata(context.Background(), s.Client(), s.URL); err == nil {
				t.Fatalf("%s: want error", name)
			}
		}()
	}

	// Array methods resolve through ResolveCCAuthMethod; scalar
	// token_endpoint_auth_method is ignored (json:"-").
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token_endpoint":"https://iss/t","token_endpoint_auth_method":"client_secret_basic","token_endpoint_auth_methods_supported":["client_secret_post","none"]}`))
	}))
	defer s2.Close()
	md2, err := DiscoverClientCredentialsMetadata(context.Background(), s2.Client(), s2.URL)
	if err != nil {
		t.Fatal(err)
	}
	if md2.TokenEndpointAuthMethod != "client_secret_post" {
		t.Fatalf("scalar must be ignored: %q", md2.TokenEndpointAuthMethod)
	}

	// Canceled ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverClientCredentialsMetadata(ctx, srv.Client(), srv.URL); err == nil {
		t.Fatal("canceled ctx must error")
	}
}

func TestResolveCCAuthMethodExtended(t *testing.T) {
	if got := ResolveCCAuthMethod([]string{"client_secret_basic", "none"}); got != "client_secret_basic" {
		t.Fatalf("basic+none: %q", got)
	}
	if got := ResolveCCAuthMethod([]string{"client_secret_basic", "client_secret_post", "none"}); got != "client_secret_post" {
		t.Fatalf("basic+post+none: %q", got)
	}
	if got := ResolveCCAuthMethod([]string{"", "none", "private_key_jwt"}); got != "client_secret_post" {
		t.Fatalf("unknown/empty: %q", got)
	}
	if got := ResolveCCAuthMethod([]string{}); got != "client_secret_post" {
		t.Fatalf("empty: %q", got)
	}
	if got := ResolveCCAuthMethod([]string{"client_secret_basic", "client_secret_basic"}); got != "client_secret_basic" {
		t.Fatalf("dup basic: %q", got)
	}
}

func TestBasicEncodeFull(t *testing.T) {
	if got := basicEncode("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"); got != "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~" {
		t.Fatalf("unreserved changed: %q", got)
	}
	cases := map[string]string{
		" ": "%20", "+": "%2B", "/": "%2F", "=": "%3D",
		"%": "%25", "&": "%26", ":": "%3A", "@": "%40",
		"é": "%C3%A9",
	}
	for in, want := range cases {
		if got := basicEncode(in); got != want {
			t.Errorf("basicEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := basicEncode(""); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestCCTokenPost(t *testing.T) {
	var form url.Values
	var ct, accept, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		ct, accept, auth = r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"acc","expires_in":10}`))
	}))
	defer srv.Close()
	md := &ClientCredentialsMetadata{TokenEndpoint: srv.URL, TokenEndpointAuthMethod: "client_secret_post"}
	access, refresh, exp, err := ClientCredentialsToken(context.Background(), srv.Client(), md, "id", "sec", "s", "res")
	if err != nil {
		t.Fatal(err)
	}
	if access != "acc" || exp != 10 || refresh != "" {
		t.Fatalf("tok: %q %q %d", access, refresh, exp)
	}
	if form.Get("grant_type") != "client_credentials" {
		t.Fatalf("grant: %v", form)
	}
	if form.Get("client_id") != "id" || form.Get("client_secret") != "sec" {
		t.Fatalf("creds: %v", form)
	}
	if form.Get("scope") != "s" || form.Get("resource") != "res" {
		t.Fatalf("scope/resource: %v", form)
	}
	if ct != "application/x-www-form-urlencoded" || accept != "application/json" {
		t.Fatalf("headers: %q %q", ct, accept)
	}
	if auth != "" {
		t.Fatalf("post must not send Authorization: %q", auth)
	}
	// Empty scope/resource omitted.
	form = nil
	md2 := &ClientCredentialsMetadata{TokenEndpoint: srv.URL} // empty method defaults to post
	if _, _, _, err := ClientCredentialsToken(context.Background(), srv.Client(), md2, "id", "sec", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := form["scope"]; ok {
		t.Fatalf("scope must be absent: %v", form)
	}
	if _, ok := form["resource"]; ok {
		t.Fatalf("resource must be absent: %v", form)
	}
	// Single grant_type value.
	if len(form["grant_type"]) != 1 {
		t.Fatalf("grant_type count: %v", form)
	}
}

func TestCCTokenBasic(t *testing.T) {
	var body, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, auth = string(b), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a"}`))
	}))
	defer srv.Close()
	md := &ClientCredentialsMetadata{TokenEndpoint: srv.URL, TokenEndpointAuthMethod: "client_secret_basic"}
	if _, _, _, err := ClientCredentialsToken(context.Background(), srv.Client(), md, "my id", "s+c/ret=", "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("want Basic Authorization, got %q", auth)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		t.Fatalf("Authorization not base64: %q", auth)
	}
	want := basicEncode("my id") + ":" + basicEncode("s+c/ret=")
	if string(dec) != want {
		t.Fatalf("Basic body: %q want %q", dec, want)
	}
	vals, _ := url.ParseQuery(body)
	if vals.Get("client_id") != "my id" {
		t.Fatalf("body client_id: %q", body)
	}
	if vals.Get("client_secret") != "" {
		t.Fatalf("basic must not send secret in body: %q", body)
	}
}

func TestCCTokenErrors(t *testing.T) {
	mk := func(code int, body string) (*httptest.Server, *ClientCredentialsMetadata) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
		return s, &ClientCredentialsMetadata{TokenEndpoint: s.URL, TokenEndpointAuthMethod: "client_secret_post"}
	}
	// 400 echoes status + body (documents: secret is already in the request,
	// but the raw body lands in the error string).
	s, md := mk(400, "bad stuff")
	defer s.Close()
	_, _, _, err := ClientCredentialsToken(context.Background(), s.Client(), md, "id", "s", "", "")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad stuff") {
		t.Fatalf("400: %v", err)
	}
	// 200 bad JSON.
	s2, md2 := mk(200, "{nope")
	defer s2.Close()
	if _, _, _, err := ClientCredentialsToken(context.Background(), s2.Client(), md2, "id", "s", "", ""); err == nil {
		t.Fatal("bad JSON must error")
	}
	// 200 no access token.
	s3, md3 := mk(200, `{"refresh_token":"r"}`)
	defer s3.Close()
	if _, _, _, err := ClientCredentialsToken(context.Background(), s3.Client(), md3, "id", "s", "", ""); err == nil {
		t.Fatal("missing access token must error")
	}
	// Canceled ctx against a slow server.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	mdSlow := &ClientCredentialsMetadata{TokenEndpoint: slow.URL, TokenEndpointAuthMethod: "client_secret_post"}
	if _, _, _, err := ClientCredentialsToken(ctx, slow.Client(), mdSlow, "id", "s", "", ""); err == nil {
		t.Fatal("canceled ctx must error")
	}
	// >64KiB body truncated: server returns 400 with 70KiB; error must be bounded.
	big := strings.Repeat("z", 70*1024)
	s4, md4 := mk(400, big)
	defer s4.Close()
	_, _, _, err = ClientCredentialsToken(context.Background(), s4.Client(), md4, "id", "s", "", "")
	if err == nil || len(err.Error()) > 70*1024 {
		t.Fatalf("truncation: len=%d", len(err.Error()))
	}
}

func TestSaveClientCredentialsToken(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "cc")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := SaveClientCredentialsToken(ctx, s, "https://iss", "acc", "ref", 90); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Issuer != "https://iss" || got.AccessToken != "acc" || got.RefreshToken != "ref" {
		t.Fatalf("saved: %+v", got)
	}
	if got.TokenType != "Bearer" || got.ExpiresIn != 90 {
		t.Fatalf("type/expiry: %+v", got)
	}
	if got.ExpiresAt.IsZero() || time.Until(got.ExpiresAt) > 91*time.Second || time.Until(got.ExpiresAt) < 80*time.Second {
		t.Fatalf("ExpiresAt: %v", got.ExpiresAt)
	}
	if !s.HasRefreshToken(ctx) {
		t.Fatal("HasRefreshToken")
	}
	// Zero expiresIn: no ExpiresAt.
	s2, err := NewStore(dir, "cc0")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveClientCredentialsToken(ctx, s2, "https://iss", "a", "", 0); err != nil {
		t.Fatal(err)
	}
	got2, err := s2.GetToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got2.ExpiresAt.IsZero() {
		t.Fatalf("zero expiry must not set ExpiresAt: %v", got2.ExpiresAt)
	}
}
