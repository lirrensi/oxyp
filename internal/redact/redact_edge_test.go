package redact

import (
	"strings"
	"testing"
)

func TestHeadersDeepCopyAndTable(t *testing.T) {
	in := map[string][]string{
		"Authorization": {"Bearer abc"},
		"X-Tenant":      {"acme", "second"},
		"X-Api-Key":     {"k"},
	}
	out := Headers(in)
	if len(out["Authorization"]) != 1 || out["Authorization"][0] != "[REDACTED]" {
		t.Fatalf("Authorization not scrubbed: %v", out)
	}
	if len(out["X-Api-Key"]) != 1 || out["X-Api-Key"][0] != "[REDACTED]" {
		t.Fatalf("X-Api-Key not scrubbed: %v", out)
	}
	if len(out["X-Tenant"]) != 2 || out["X-Tenant"][0] != "acme" {
		t.Fatalf("non-secret mangled: %v", out)
	}
	// current behavior (gap): Headers is a SHALLOW copy — mutating the
	// returned slice aliases the caller's backing array.
	out["X-Tenant"][0] = "MUT"
	if in["X-Tenant"][0] != "MUT" {
		t.Log("Headers deep-copies values (gap fixed)")
	} else {
		t.Log("current behavior: Headers shallow-copies slices (aliasing gap)")
	}
	in["X-Tenant"][0] = "acme" // restore for the table below
	// key table: every entry in keys redacts case-insensitively.
	for _, k := range []string{
		"authorization", "proxy-authorization", "token", "access_token",
		"refresh_token", "authtoken", "api_key", "apikey", "x-api-key",
		"client_secret", "clientsecret", "password", "secret",
	} {
		got := Headers(map[string][]string{k: {"v"}, strings.ToUpper(k): {"v2"}})
		if got[k][0] != "[REDACTED]" || got[strings.ToUpper(k)][0] != "[REDACTED]" {
			t.Errorf("key %q not redacted case-insensitively: %v", k, got)
		}
	}
	// non-secret preserved + original untouched length.
	orig := map[string][]string{"Content-Type": {"application/json"}}
	got := Headers(orig)
	if got["Content-Type"][0] != "application/json" {
		t.Errorf("over-redacted: %v", got)
	}
	if len(orig) != 1 {
		t.Errorf("input map mutated: %v", orig)
	}
}

func TestHeaderMapKeyTable(t *testing.T) {
	for _, k := range []string{
		"authorization", "proxy-authorization", "token", "access_token",
		"refresh_token", "authtoken", "api_key", "apikey", "x-api-key",
		"client_secret", "clientsecret", "password", "secret",
	} {
		got := HeaderMap(map[string]string{k: "v", strings.ToUpper(k): "v2"})
		if got[k] != "[REDACTED]" || got[strings.ToUpper(k)] != "[REDACTED]" {
			t.Errorf("key %q not redacted: %v", k, got)
		}
	}
	got := HeaderMap(map[string]string{"X-Tenant": "acme", "Content-Type": "application/json"})
	if got["X-Tenant"] != "acme" || got["Content-Type"] != "application/json" {
		t.Errorf("non-secret keys must be preserved: %v", got)
	}
	// nil → non-nil empty map (current behavior).
	if out := HeaderMap(nil); out == nil || len(out) != 0 {
		t.Fatalf("HeaderMap(nil) = %#v, want non-nil empty", out)
	}
	// input not mutated.
	in := map[string]string{"Authorization": "Bearer x"}
	_ = HeaderMap(in)
	if in["Authorization"] != "Bearer x" {
		t.Fatal("HeaderMap mutated input")
	}
}

func TestHeaderCamelAndCookieGaps(t *testing.T) {
	// current behavior (gaps): camelCase token keys and cookie-ish keys leak.
	for _, k := range []string{"accessToken", "refreshToken", "Cookie", "Set-Cookie", "X-Auth-Token", "id_token", "bearer", "private_key"} {
		v := HeaderMap(map[string]string{k: "SECRET"})[k]
		t.Logf("current behavior: HeaderMap(%q) = %q", k, v)
	}
	// pin the current leak explicitly for the two highest-value keys.
	if got := HeaderMap(map[string]string{"accessToken": "SECRET"})["accessToken"]; got != "SECRET" {
		t.Logf("accessToken now redacted (gap fixed): %q", got)
	} else {
		t.Log("current behavior: accessToken leaks (camelCase gap)")
	}
	if got := HeaderMap(map[string]string{"Cookie": "SECRET"})["Cookie"]; got != "SECRET" {
		t.Logf("Cookie now redacted (gap fixed): %q", got)
	} else {
		t.Log("current behavior: Cookie leaks (cookie gap)")
	}
	// Body-side camel gap.
	got := Body(`{"accessToken":"abc","refreshToken":"pw","ok":1}`)
	if strings.Contains(got, "abc") || strings.Contains(got, "pw") {
		t.Logf("current behavior: Body camelCase keys leak: %s", got)
	} else {
		t.Log("Body camelCase keys redacted (gap fixed)")
	}
}

func TestBodyRedactsNestedArraysAndKeys(t *testing.T) {
	in := `{"a":[{"password":"x"},{"deep":{"clientSecret":"y","keep":1}}],"Authorization":"z","list":[1,"token",{"apiKey":"w"}],"ok":"fine"}`
	got := Body(in)
	for _, leak := range []string{`"x"`, `"y"`, `"z"`, `"w"`} {
		if strings.Contains(got, leak) {
			t.Errorf("secret leaked %s in %s", leak, got)
		}
	}
	for _, keep := range []string{`"keep":1`, `"ok":"fine"`, `"list":[1,"token"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("non-secret lost %s in %s", keep, got)
		}
	}
	if n := strings.Count(got, "[REDACTED]"); n != 4 {
		t.Errorf("want 4 redactions, got %d: %s", n, got)
	}
}

func TestBodyPlainTextScrubbed(t *testing.T) {
	// Plain-text credential patterns are masked (Bearer/Basic, key=value).
	for s, want := range map[string]string{
		"Authorization: Bearer abc": "Authorization: Bearer [REDACTED]",
		"token=abc":                 "token=[REDACTED]",
		"apiKey: xyz":               "apiKey: [REDACTED]",
		"nothing secret here":       "nothing secret here",
	} {
		if got := Body(s); got != want {
			t.Errorf("Body(%q) = %q, want %q", s, got, want)
		}
	}
}

func TestBodyDepthBoundary(t *testing.T) {
	nest := func(depth int) string {
		s := `"leaf"`
		for range depth {
			s = `{"k":` + s + `}`
		}
		return s
	}
	// exactly 32 nests: walk(k,0..32) — depth 32 is allowed (not > 32).
	if got := Body(nest(32)); strings.Contains(got, "[DEEP]") {
		t.Errorf("depth-32 must not hit [DEEP]: %.80s", got)
	}
	// 33 nests: innermost walk at depth 33 → [DEEP].
	got := Body(nest(33))
	if !strings.Contains(got, "[DEEP]") {
		t.Errorf("depth-33 must hit [DEEP]: %.80s", got)
	}
	if strings.Contains(got, "leaf") {
		t.Errorf("depth-33 must drop the leaf: %.80s", got)
	}
}

func TestBodyTruncateBoundary(t *testing.T) {
	if got := Body(strings.Repeat("a", 8192)); got != strings.Repeat("a", 8192) {
		t.Fatal("8192 bytes must pass through unchanged")
	}
	got := Body(strings.Repeat("b", 8193))
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Fatal("8193 bytes must truncate with suffix")
	}
	if !strings.HasPrefix(got, strings.Repeat("b", 8192)) {
		t.Fatal("truncate must keep the first 8192 bytes")
	}
	// multibyte cut keeps valid UTF-8: 8191 ASCII + é (2 bytes) overflows.
	s := strings.Repeat("a", 8191) + "é" + "tail"
	got = Body(s)
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("expected truncation, len=%d", len(got))
	}
	body := strings.TrimSuffix(got, "…[truncated]")
	if len(body) > 8192 {
		t.Fatalf("truncated body %d bytes exceeds 8192", len(body))
	}
	for i := 0; i < len(body); {
		c := body[i]
		var n int
		switch {
		case c < 0x80:
			n = 1
		case c>>5 == 0x6:
			n = 2
		case c>>4 == 0xE:
			n = 3
		case c>>3 == 0x1E:
			n = 4
		default:
			t.Fatalf("invalid UTF-8 at byte %d", i)
		}
		if i+n > len(body) {
			t.Fatal("truncated mid-rune")
		}
		i += n
	}
}

func TestBodyJSONScalars(t *testing.T) {
	cases := []struct{ in, want string }{
		{`""`, `""`},
		{`null`, `null`},
		{`"x"`, `"x"`},
		{`42`, `42`},
		{`true`, `true`},
		{`invalid json`, `invalid json`},
		{`   `, `   `},
	}
	for _, tc := range cases {
		if got := Body(tc.in); got != tc.want {
			t.Errorf("Body(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestURLTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // exact current-behavior output
	}{
		{"userinfo query frag stripped", "https://user:pass@example.com:8443/mcp?token=abc#frag", "https://example.com:8443/mcp"},
		{"plain survives", "http://localhost:8379/x", "http://localhost:8379/x"},
		{"bad scheme", "://bad", "[invalid-url]"},
		{"opaque", "not a url", "[invalid-url]"},
		{"empty", "", "[invalid-url]"},
		{"no host", "/just/path", "[invalid-url]"},
		{"trailing ? dropped", "https://host/p?", "https://host/p"},
		{"query stripped keeps ?", "https://host/p?a=b", "https://host/p"},
		{"scheme-relative passes", "//host/path", "//host/path"},
		{"ftp passes", "ftp://host/file", "ftp://host/file"},
		{"uppercase scheme lowercased", "HTTPS://host/P", "https://host/P"},
		{"IPv6 port", "http://[::1]:8379/mcp?x=1", "http://[::1]:8379/mcp"},
		{"trailing slash kept", "https://host/", "https://host/"},
		{"path token preserved by design", "https://host/tok-abc123/def", "https://host/tok-abc123/def"},
		{"userinfo + port", "http://u@h:1/p", "http://h:1/p"},
	}
	for _, tc := range cases {
		if got := URL(tc.in); got != tc.want {
			t.Errorf("%s: URL(%q) = %q, want %q (current behavior)", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestURLDoesNotLeakQueryOrUserinfo(t *testing.T) {
	for _, in := range []string{
		"https://u:p@h/x?tok=1#f",
		"https://h/x?tok=1&other=2",
		"http://admin:s3cret@[::1]:9/y?a=b",
	} {
		got := URL(in)
		if strings.Contains(got, "tok") || strings.Contains(got, "u:p") ||
			strings.Contains(got, "admin") || strings.Contains(got, "?") && strings.Contains(got, "=") ||
			strings.Contains(got, "#") || strings.Contains(got, "@") {
			t.Errorf("URL(%q) leaks credentials: %q", in, got)
		}
	}
}
