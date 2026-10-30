package oauth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRandomStateAndPKCE(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		s, err := RandomState()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 32 {
			t.Fatalf("state len: %q", s)
		}
		for _, c := range s {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Fatalf("state not hex: %q", s)
			}
		}
		seen[s] = true
	}
	if len(seen) != 1000 {
		t.Fatalf("states not unique: %d/1000", len(seen))
	}

	vseen := map[string]bool{}
	for range 100 {
		v, err := CodeVerifier()
		if err != nil {
			t.Fatal(err)
		}
		if len(v) != 43 {
			t.Fatalf("verifier len: %d (%q)", len(v), v)
		}
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(b) != 32 {
			t.Fatalf("verifier decodes to 32 bytes: %v", err)
		}
		vseen[v] = true
	}
	if len(vseen) != 100 {
		t.Fatal("verifiers not unique")
	}
	// RFC 7636 Appendix B vector.
	if got := CodeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("RFC7636 vector: %q", got)
	}
	if got := CodeChallenge(CodeChallenge("x")); len(got) != 43 {
		t.Fatal("challenge chaining produced wrong length")
	}
}

func TestNormalizeScheme(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "Bearer"},
		{"bearer", "Bearer"},
		{"BEARER", "Bearer"},
		{"bEaReR", "Bearer"},
		{"Bearer", "Bearer"},
		{"Basic", "Basic"},
		{"DPoP", "DPoP"},
		{"bearer ", "bearer "}, // trailing space: unchanged (current behavior)
		{"béarer", "béarer"},   // non-ASCII: unchanged
		{"MAC", "MAC"},
	}
	for _, tc := range cases {
		if got := NormalizeScheme(tc.in); got != tc.want {
			t.Errorf("NormalizeScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExpandRedirectURI(t *testing.T) {
	cases := []struct {
		tpl  string
		port int
		want string
	}{
		{"", 8378, "http://localhost:8378/callback"},
		{"", 0, "http://localhost:0/callback"},
		{"http://localhost:{port}/callback", 9000, "http://localhost:9000/callback"},
		{"http://127.0.0.1:{port}/a/{port}", 1, "http://127.0.0.1:1/a/1"},
		{"http://localhost:8378/callback", 9999, "http://localhost:8378/callback"},
		{"custom", 1234, "custom"},
	}
	for _, tc := range cases {
		if got := ExpandRedirectURI(tc.tpl, tc.port); got != tc.want {
			t.Errorf("ExpandRedirectURI(%q,%d) = %q, want %q", tc.tpl, tc.port, got, tc.want)
		}
	}
}

func TestParseCallbackInput(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantCode  string
		wantState string
	}{
		{"bare code", "abc", "abc", ""},
		{"full URL", "http://localhost:8378/callback?code=CODE&state=ST", "CODE", "ST"},
		{"query only", "?code=a&state=b", "a", "b"},
		{"bare query", "code=a&state=b", "a", "b"},
		{"percent-encoded state", "http://x/cb?code=c&state=a%20b", "c", "a b"},
		{"error only", "http://x/cb?error=access_denied", "", ""}, // current behavior: error dropped, state empty
		{"empty", "", "", ""},
		{"whitespace", "   ", "", ""},
		{"non-query kv", "abc=def", "", ""}, // conflated with query (current behavior)
		{"duplicate code first wins", "code=first&code=second&state=s", "first", "s"},
		{"fragment ignored", "http://x/cb#code=frag", "", ""}, // fragment is not a query (current behavior)
		{"code with spaces trimmed", "  bare123  ", "bare123", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, state := ParseCallbackInput(tc.in)
			if code != tc.wantCode || state != tc.wantState {
				t.Fatalf("ParseCallbackInput(%q) = (%q,%q), want (%q,%q)",
					tc.in, code, state, tc.wantCode, tc.wantState)
			}
		})
	}
	if code, _ := ParseCallbackInput(strings.Repeat("x", 10000)); code == "" {
		t.Fatal("long bare code must pass through")
	}
}
