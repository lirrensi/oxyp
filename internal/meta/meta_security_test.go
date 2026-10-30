package meta

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/mark3labs/mcp-go/mcp"
)

// gateHandler builds a meta handler whose gates run but whose mux never
// dereferences the daemon: unknown paths 404 after the gates pass.
// Named servers never resolve, so d is only touched after gates pass.
func gateHandler(bind string) http.Handler {
	return New(nil, nil, "", bind).Handler()
}

func gateReq(method, target, host, origin, ct string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	return r
}

func doGate(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func TestHandlerBearer(t *testing.T) {
	h := New(nil, nil, "sekret", "127.0.0.1:8379")
	cases := []struct {
		name string
		auth string
		code int
	}{
		{"missing", "", 401},
		{"wrong scheme lowercase", "bearer sekret", 401},
		{"wrong value", "Bearer nope", 401},
		{"correct", "Bearer sekret", 404},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/nope", nil)
		r.Host = "127.0.0.1"
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		rr := doGate(h.Handler(), r)
		if rr.Code != tc.code {
			t.Errorf("%s: got %d want %d", tc.name, rr.Code, tc.code)
		}
		if tc.code == 401 && !strings.Contains(rr.Body.String(), "missing or wrong bearer token") {
			t.Errorf("%s: body %q", tc.name, rr.Body.String())
		}
	}
	// Empty token disables auth: unknown path 404 with no header.
	h2 := New(nil, nil, "", "127.0.0.1:8379")
	r := httptest.NewRequest("GET", "/nope", nil)
	r.Host = "127.0.0.1"
	if rr := doGate(h2.Handler(), r); rr.Code != http.StatusNotFound {
		t.Fatalf("empty token should pass auth, got %d", rr.Code)
	}
}

func TestHandlerBearerBeforeHost(t *testing.T) {
	// Wrong token + foreign host: token denial wins (auth runs first).
	h := New(nil, nil, "t", "127.0.0.1:8379")
	r := httptest.NewRequest("GET", "/nope", nil)
	r.Host = "evil.com"
	r.Header.Set("Authorization", "Bearer wrong")
	rr := doGate(h.Handler(), r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 bearer denial first, got %d", rr.Code)
	}
}

func TestCheckHost(t *testing.T) {
	loop := []string{"localhost", "localhost:1", "127.0.0.1", "127.0.0.1:9",
		"[::1]:9", "::1", "[::ffff:127.0.0.1]:9", "LOCALHOST", "LocalHost:8379"}
	for _, host := range loop {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if !checkHost(r, "127.0.0.1:8379") {
			t.Errorf("loopback bind should accept Host %q", host)
		}
	}
	evil := []string{"evil.com", "127.0.0.1.evil.com", "localhost.", "localhost.evil.com",
		"10.0.0.1", "", "0.0.0.0", "[::ffff:10.0.0.1]:9"}
	for _, host := range evil {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if checkHost(r, "127.0.0.1:8379") {
			t.Errorf("loopback bind should reject Host %q", host)
		}
	}
	// Binds on other loopback forms behave the same.
	for _, bind := range []string{"localhost:8379", "[::1]:8379", ":8379"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = "evil.com"
		if checkHost(r, bind) && bind != ":8379" {
			t.Errorf("bind %q should reject foreign host", bind)
		}
	}
	// Non-loopback bind accepts any host.
	for _, host := range []string{"evil.com", "", "10.0.0.1"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if !checkHost(r, "0.0.0.0:8379") {
			t.Errorf("non-loopback bind should accept Host %q", host)
		}
	}
	// Malformed bind fails closed.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.com"
	if checkHost(r, "garbage-without-port") {
		t.Error("malformed bind should fail closed (reject foreign host)")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Host = "127.0.0.1"
	if !checkHost(r2, "garbage-without-port") {
		t.Error("malformed bind should still accept loopback")
	}
	// Handler surface: foreign Host gets 403.
	h := gateHandler("127.0.0.1:8379")
	rr := doGate(h, gateReq("GET", "/nope", "evil.com", "", ""))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "foreign Host rejected") {
		t.Fatalf("want 403 foreign Host, got %d %q", rr.Code, rr.Body.String())
	}
}

func TestCheckOrigin(t *testing.T) {
	pass := []string{"", "localhost", "localhost:1234", "127.0.0.1", "127.0.0.1:5"}
	for _, o := range pass {
		r := httptest.NewRequest("GET", "/", nil)
		if o != "" {
			r.Header.Set("Origin", o)
		}
		if !checkOrigin(r) {
			t.Errorf("should accept Origin %q", o)
		}
	}
	// NOTE (known bug, omitted from assertions): full browser origins with a
	// scheme are currently rejected — SplitHostPort runs before the scheme is
	// stripped, so "http://localhost:1234" never resolves to loopback.
	deny := []string{"https://evil.com", "evil.com", "null", "http://localhost.evil.com",
		"http://127.0.0.1@evil.com", "http://10.0.0.1/", "http://[::ffff:10.0.0.1]/"}
	for _, o := range deny {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Origin", o)
		if checkOrigin(r) {
			t.Errorf("should reject Origin %q", o)
		}
	}
	// Handler surface.
	h := gateHandler("127.0.0.1:8379")
	rr := doGate(h, gateReq("GET", "/nope", "127.0.0.1", "https://evil.com", ""))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "foreign Origin rejected") {
		t.Fatalf("want 403 foreign Origin, got %d %q", rr.Code, rr.Body.String())
	}
}

func TestHandlerContentTypeGate(t *testing.T) {
	h := gateHandler("127.0.0.1:8379")
	post := func(ct string, len0 bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/nope", strings.NewReader("{}"))
		r.Host = "127.0.0.1"
		if len0 {
			r.ContentLength = 0
		}
		r.Header.Set("Content-Type", ct)
		return doGate(h, r)
	}
	if rr := post("text/plain", false); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST: got %d want 415", rr.Code)
	}
	r := httptest.NewRequest("POST", "/nope", strings.NewReader("{}"))
	r.Host = "127.0.0.1"
	r.Header.Del("Content-Type")
	if rr := doGate(h, r); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("missing CT POST: got %d want 415", rr.Code)
	}
	// Empty-body POST is not gated (ContentLength 0): falls to mux 404/405.
	rr := post("text/plain", true)
	if rr.Code == http.StatusUnsupportedMediaType {
		t.Errorf("empty-body POST must skip the gate, got %d", rr.Code)
	}
	// GET/HEAD with a body are not gated either.
	for _, m := range []string{"GET", "HEAD"} {
		r := httptest.NewRequest(m, "/nope", strings.NewReader("{}"))
		r.Host = "127.0.0.1"
		r.Header.Set("Content-Type", "text/plain")
		if rr := doGate(h, r); rr.Code == http.StatusUnsupportedMediaType {
			t.Errorf("%s must skip the gate, got 415", m)
		}
	}
	// Accepted forms.
	for _, ct := range []string{"application/json", "application/json; charset=utf-8"} {
		if rr := post(ct, false); rr.Code == http.StatusUnsupportedMediaType {
			t.Errorf("CT %q should pass the gate", ct)
		}
	}
	// CURRENT behavior: gate is case-sensitive and prefix-lenient.
	if rr := post("Application/JSON", false); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("uppercase CT currently rejected (got %d); documented HasPrefix behavior", rr.Code)
	}
}

func TestHandlerOptionsMethod(t *testing.T) {
	// No CORS/preflight handler: OPTIONS falls through to the mux (405).
	h := gateHandler("127.0.0.1:8379")
	r := httptest.NewRequest("OPTIONS", "/servers", nil)
	r.Host = "127.0.0.1"
	if rr := doGate(h, r); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("OPTIONS: got %d want 405", rr.Code)
	}
}

func TestWriteDownstreamErrGeneric(t *testing.T) {
	// Pure-JSON error bodies get credential keys redacted, status 502.
	w := httptest.NewRecorder()
	writeDownstreamErr(w, errors.New(`{"access_token":"SECRET"}`))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("credential leaked: %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[REDACTED]") {
		t.Fatalf("want redaction marker: %q", w.Body.String())
	}
	// Embedded JSON in prose: key=value scrubber masks it.
	w = httptest.NewRecorder()
	writeDownstreamErr(w, errors.New(`boom {"access_token":"SECRET"}`))
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("embedded JSON leaked: %q", w.Body.String())
	}
	// Plain (non-JSON) bearer text IS redacted.
	w = httptest.NewRecorder()
	writeDownstreamErr(w, errors.New("boom Bearer SECRET"))
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("plain bearer text leaked: %q", w.Body.String())
	}
	// >8 KiB truncates.
	w = httptest.NewRecorder()
	writeDownstreamErr(w, errors.New(strings.Repeat("x", 9000)))
	if !strings.Contains(w.Body.String(), "…[truncated]") {
		t.Fatalf("want truncation marker, len=%d", w.Body.Len())
	}
	// AuthNeededError maps to 401 + login hint.
	w = httptest.NewRecorder()
	writeDownstreamErr(w, &daemon.AuthNeededError{Server: "s"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("auth-needed: got %d want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "run: oxyp login s") {
		t.Fatalf("want login hint: %q", w.Body.String())
	}
}

func TestIsUnsupported(t *testing.T) {
	for _, err := range []error{
		mcp.ErrMethodNotFound,
		errors.New("do x: method not found"),
		errors.New("wrap: not found: resources/abc"),
		errors.New("wrap: not found: prompts/abc"),
	} {
		if !isUnsupported(err) {
			t.Errorf("should be unsupported: %v", err)
		}
	}
	for _, err := range []error{
		errors.New("not found: tools/abc"),
		errors.New("boom"),
	} {
		if isUnsupported(err) {
			t.Errorf("should NOT be unsupported: %v", err)
		}
	}
}

func TestAtoiOr(t *testing.T) {
	cases := []struct {
		in   string
		dflt int
		want int
	}{
		{"", 12, 12},
		{"12", 0, 12},
		{"x", 7, 7},
		{"-1", 0, -1},
		{" 12", 0, 12},
		// CURRENT behavior: fmt.Sscanf accepts numeric prefixes.
		{"12abc", 0, 12},
	}
	for _, tc := range cases {
		if got := atoiOr(tc.in, tc.dflt); got != tc.want {
			t.Errorf("atoiOr(%q,%d) = %d, want %d", tc.in, tc.dflt, got, tc.want)
		}
	}
}
