package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func deviceMetaHandler(doc map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

func TestDiscoverDeviceMetadata(t *testing.T) {
	doc := map[string]any{
		"issuer": "https://iss", "device_authorization_endpoint": "https://iss/dev",
		"token_endpoint": "https://iss/tok", "registration_endpoint": "https://iss/reg",
	}
	srv := httptest.NewServer(deviceMetaHandler(doc))
	defer srv.Close()
	md, err := DiscoverDeviceMetadata(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if md.DeviceAuthorizationEndpoint != "https://iss/dev" || md.TokenEndpoint != "https://iss/tok" {
		t.Fatalf("fields: %+v", md)
	}
	// Trailing slash issuer still hits the well-known path.
	var path string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv2.Close()
	if _, err := DiscoverDeviceMetadata(context.Background(), srv2.Client(), srv2.URL+"/"); err != nil {
		t.Fatal(err)
	}
	if path != "/.well-known/oauth-authorization-server" {
		t.Fatalf("path: %s", path)
	}
	// Non-200, bad JSON, missing device endpoint.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	if _, err := DiscoverDeviceMetadata(context.Background(), bad.Client(), bad.URL); err == nil {
		t.Fatal("500 must error")
	}
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{bad`))
	}))
	defer g.Close()
	if _, err := DiscoverDeviceMetadata(context.Background(), g.Client(), g.URL); err == nil {
		t.Fatal("bad JSON must error")
	}
	noDev := httptest.NewServer(deviceMetaHandler(map[string]any{"issuer": "i", "token_endpoint": "https://i/t"}))
	defer noDev.Close()
	_, err = DiscoverDeviceMetadata(context.Background(), noDev.Client(), noDev.URL)
	if err == nil || !strings.Contains(err.Error(), "device authorization") {
		t.Fatalf("missing device endpoint: %v", err)
	}
	// Missing issuer/token_endpoint fail fast (needed for refresh binding).
	noIss := httptest.NewServer(deviceMetaHandler(map[string]any{"device_authorization_endpoint": "https://i/d"}))
	defer noIss.Close()
	if _, err := DiscoverDeviceMetadata(context.Background(), noIss.Client(), noIss.URL); err == nil {
		t.Fatal("missing issuer must fail fast")
	}
}

func TestRegisterDeviceClientStatic(t *testing.T) {
	md := &DeviceMetadata{DeviceAuthorizationEndpoint: "https://i/d", TokenEndpoint: "https://i/t"}
	// Static client returns immediately; nil httpClient proves no request.
	dc, err := RegisterDeviceClient(context.Background(), nil, md, "static-id", "static-sec")
	if err != nil {
		t.Fatal(err)
	}
	if dc.ClientID != "static-id" || dc.ClientSecret != "static-sec" {
		t.Fatalf("static: %+v", dc)
	}
}

func TestRegisterDeviceClientDCR(t *testing.T) {
	var gotBody map[string]any
	var gotCT string
	var status = 201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"client_id":"dyn","client_secret":"s"}`))
	}))
	defer srv.Close()
	md := &DeviceMetadata{RegistrationEndpoint: srv.URL}
	dc, err := RegisterDeviceClient(context.Background(), srv.Client(), md, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if dc.ClientID != "dyn" || dc.ClientSecret != "s" {
		t.Fatalf("dcr: %+v", dc)
	}
	if gotBody["client_name"] != "oxyp" || gotBody["token_endpoint_auth_method"] != "client_secret_post" {
		t.Fatalf("dcr body: %v", gotBody)
	}
	if grants, _ := gotBody["grant_types"].([]any); len(grants) != 2 {
		t.Fatalf("grant_types: %v", gotBody)
	}
	if resps, _ := gotBody["response_types"].([]any); len(resps) != 0 {
		t.Fatalf("response_types: %v", gotBody)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type: %q", gotCT)
	}
	status = 200 // 200 also accepted
	if _, err := RegisterDeviceClient(context.Background(), srv.Client(), md, "", ""); err != nil {
		t.Fatalf("201/200: %v", err)
	}
	// No registration endpoint.
	if _, err := RegisterDeviceClient(context.Background(), srv.Client(), &DeviceMetadata{}, "", ""); err == nil {
		t.Fatal("missing reg endpoint must error")
	}
	// 400 includes body.
	status = 400
	if _, err := RegisterDeviceClient(context.Background(), srv.Client(), md, "", ""); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Fatalf("400: %v", err)
	}
}

func TestRequestDeviceCode(t *testing.T) {
	var form url.Values
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"d","user_code":"u","verification_uri":"https://v","expires_in":0,"interval":0}`))
	}))
	defer srv.Close()
	md := &DeviceMetadata{DeviceAuthorizationEndpoint: srv.URL}
	da, err := RequestDeviceCode(context.Background(), srv.Client(), md,
		&DeviceClient{ClientID: "cid", ClientSecret: "sec"}, "scope", "res")
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("client_id") != "cid" || form.Get("scope") != "scope" || form.Get("resource") != "res" {
		t.Fatalf("form: %v", form)
	}
	if auth == "" {
		t.Fatal("secret set: want Basic header")
	}
	if da.Interval != 5 || da.ExpiresIn != 300 {
		t.Fatalf("defaults: %+v", da)
	}
	// Missing fields error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"d"}`))
	}))
	defer bad.Close()
	mdBad := &DeviceMetadata{DeviceAuthorizationEndpoint: bad.URL}
	if _, err := RequestDeviceCode(context.Background(), bad.Client(), mdBad, &DeviceClient{ClientID: "c"}, "", ""); err == nil {
		t.Fatal("missing fields must error")
	}
	// Non-200.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte("nope"))
	}))
	defer fail.Close()
	mdFail := &DeviceMetadata{DeviceAuthorizationEndpoint: fail.URL}
	if _, err := RequestDeviceCode(context.Background(), fail.Client(), mdFail, &DeviceClient{ClientID: "c"}, "", ""); err == nil {
		t.Fatal("non-200 must error")
	}
}

func TestPollPendingThenSuccess(t *testing.T) {
	var polls atomic.Int32
	var shownURI, shownCode string
	var showCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		switch n {
		case 1:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","refresh_token":"ref","expires_in":5}`))
		}
	}))
	defer srv.Close()
	md := &DeviceMetadata{TokenEndpoint: srv.URL}
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "USER", VerificationURI: "https://v", ExpiresIn: 60, Interval: 1}
	access, refresh, exp, err := PollDeviceToken(context.Background(), srv.Client(), md,
		&DeviceClient{ClientID: "c"}, da, time.Minute, "",
		func(uri, code string) { showCalls.Add(1); shownURI, shownCode = uri, code })
	if err != nil {
		t.Fatal(err)
	}
	if access != "tok" || refresh != "ref" || exp != 5 {
		t.Fatalf("tok: %q %q %d", access, refresh, exp)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls: %d", polls.Load())
	}
	if showCalls.Load() != 1 || shownURI != "https://v" || shownCode != "USER" {
		t.Fatalf("showCode: %d %q %q", showCalls.Load(), shownURI, shownCode)
	}
}

func TestPollTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{"denied", 400, `{"error":"access_denied"}`, "access_denied"},
		{"expired", 400, `{"error":"expired_token"}`, "expired_token"},
		{"unknown-error", 400, `{"error":"weird"}`, "HTTP 400"},
		{"bad-status", 500, `oops`, "HTTP 500"},
		{"bad-json", 200, `{bad`, "device token"},
		{"no-token", 200, `{"refresh_token":"r"}`, "no access token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			md := &DeviceMetadata{TokenEndpoint: srv.URL}
			da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", VerificationURI: "https://v", ExpiresIn: 60, Interval: 1}
			_, _, _, err := PollDeviceToken(context.Background(), srv.Client(), md,
				&DeviceClient{ClientID: "c"}, da, time.Minute, "", func(string, string) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err: %v want %q", err, tc.want)
			}
		})
	}
}

func TestPollExpiryAndCtx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer srv.Close()
	md := &DeviceMetadata{TokenEndpoint: srv.URL}
	// Interval default 5s exceeds a 100ms timeout: immediate expiry without waiting.
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", VerificationURI: "https://v", ExpiresIn: 1, Interval: 0}
	start := time.Now()
	_, _, _, err := PollDeviceToken(context.Background(), srv.Client(), md,
		&DeviceClient{ClientID: "c"}, da, 100*time.Millisecond, "", func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), "expired before approval") {
		t.Fatalf("expiry: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("expiry must be immediate, not after sleeping")
	}
	// Canceled ctx during the wait returns ctx.Err().
	da2 := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", VerificationURI: "https://v", ExpiresIn: 60, Interval: 5}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, _, err = PollDeviceToken(ctx, srv.Client(), md, &DeviceClient{ClientID: "c"}, da2, time.Minute, "", func(string, string) {})
	if err == nil {
		t.Fatal("ctx cancel must error")
	}
}

func TestPollMissingTokenEndpointDocumented(t *testing.T) {
	// Current behavior: an empty TokenEndpoint makes NewRequest fail with
	// "empty url", which the loop treats as a transport error and doubles the
	// interval until the deadline — a slow spin instead of a fast error.
	md := &DeviceMetadata{DeviceAuthorizationEndpoint: "https://i/d"} // no token endpoint
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", VerificationURI: "https://v", ExpiresIn: 2, Interval: 1}
	start := time.Now()
	_, _, _, err := PollDeviceToken(context.Background(), http.DefaultClient, md,
		&DeviceClient{ClientID: "c"}, da, 3*time.Second, "", func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), "expired before approval") {
		t.Fatalf("missing endpoint: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("must exit by deadline")
	}
}

func TestPollAuthHeader(t *testing.T) {
	// applyClientAuth uses HTTP Basic (client_secret_post comment vs Basic code
	// mismatch documented in O36); DCR registers "none" (device.go).
	var auth string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a"}`))
	}))
	defer srv.Close()
	md := &DeviceMetadata{TokenEndpoint: srv.URL}
	da := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", VerificationURI: "https://v", ExpiresIn: 60, Interval: 1}
	if _, _, _, err := PollDeviceToken(context.Background(), srv.Client(), md,
		&DeviceClient{ClientID: "cid", ClientSecret: "sec"}, da, time.Minute, "res",
		func(string, string) {}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("poll must carry Basic auth: %q", auth)
	}
	if form.Get("resource") != "res" {
		t.Fatalf("resource: %v", form)
	}
	// No secret: no Authorization header.
	auth = "unset"
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a"}`))
	}))
	defer srv2.Close()
	md2 := &DeviceMetadata{TokenEndpoint: srv2.URL}
	da2 := &DeviceAuthorization{DeviceCode: "d", UserCode: "u", VerificationURI: "https://v", ExpiresIn: 60, Interval: 1}
	if _, _, _, err := PollDeviceToken(context.Background(), srv2.Client(), md2,
		&DeviceClient{ClientID: "cid"}, da2, time.Minute, "", func(string, string) {}); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Fatalf("anonymous poll must not set Authorization: %q", auth)
	}
}
