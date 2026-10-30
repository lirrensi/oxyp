package redact

import (
	"strings"
	"testing"
)

func TestHeaderMap(t *testing.T) {
	got := HeaderMap(map[string]string{
		"Authorization": "Bearer x", "X-Tenant": "acme", "x-api-key": "k",
	})
	if got["Authorization"] != "[REDACTED]" || got["x-api-key"] != "[REDACTED]" {
		t.Fatalf("not redacted: %v", got)
	}
	if got["X-Tenant"] != "acme" {
		t.Fatalf("over-redacted: %v", got)
	}
}

func TestBodyJSON(t *testing.T) {
	got := Body(`{"access_token":"abc","nested":{"password":"pw"},"ok":1}`)
	if strings.Contains(got, "abc") || strings.Contains(got, "pw") {
		t.Fatalf("secrets leaked: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, `"ok":1`) {
		t.Fatalf("bad redact: %s", got)
	}
}

func TestBodyTruncate(t *testing.T) {
	big := strings.Repeat("a", 9000)
	got := Body(big)
	if len(got) > 8192+16 || !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("bad truncate: len=%d", len(got))
	}
}

func TestURLStrip(t *testing.T) {
	got := URL("https://user:pass@example.com:8443/mcp?token=abc#frag")
	if got != "https://example.com:8443/mcp" {
		t.Fatalf("URL = %q", got)
	}
	if URL("http://localhost:8379/x") != "http://localhost:8379/x" {
		t.Fatal("plain url must survive")
	}
	if URL("://bad") != "[invalid-url]" || URL("not a url") != "[invalid-url]" {
		t.Fatal("unparseable must be [invalid-url]")
	}
}
