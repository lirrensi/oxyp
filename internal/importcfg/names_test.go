package importcfg

import (
	"strings"
	"testing"
)

func TestSanitizeNameTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"lower", "My Server_2!", "my-server-2"},
		{"empty", "", "server"},
		{"symbols", "!!!", "server"},
		{"unicodeStripped", "Héllo Wörld", "h-llo-w-rld"},
		{"allNonASCII", "日本語", "server"},
		{"collapseDashes", "a--b__c", "a-b-c"},
		{"trimEdges", "  -hi-  ", "hi"},
		{"ipv6", "::1", "1"},
		{"alreadyClean", "srv-2", "srv-2"},
		{"dots", "a.b.c", "a-b-c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeName(tc.in); got != tc.want {
				t.Fatalf("SanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNameFromURLTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"basic", "https://company.glean.com/mcp", "company-glean-com", ""},
		{"lowercaseWww", "https://www.example.com/mcp", "example-com", ""},
		// current behavior: only lowercase "www." is stripped.
		{"upperWWWKept", "https://WWW.example.com/mcp", "www-example-com", ""},
		{"portIgnored", "https://example.com:8443/x", "example-com", ""},
		{"userinfoIgnored", "https://user:pass@example.com:443/mcp", "example-com", ""},
		{"trailingDot", "https://example.com./mcp", "example-com", ""},
		{"upperSchemeAccepted", "HTTPS://UPPER.com/m", "upper-com", ""}, // current behavior: url.Parse lowercases scheme
		{"ipv6", "http://[::1]:8080/mcp", "1", ""},
		{"subSub", "https://a.b.c.d.e/m", "a-b-c-d-e", ""},
		{"empty", "", "", "bad url"},
		{"garbage", "not a url", "", "bad url"},
		{"ftp", "ftp://x/y", "", "http(s)"},
		{"noHost", "https:///path", "", "bad url"},
		{"badIPv6", "https://[::1/mcp", "", "bad url"},
		{"badPort", "https://example.com:abc/m", "", "bad url"},
		{"padded", "  https://example.com/m  ", "example-com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NameFromURL(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("NameFromURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUniqueNameTable(t *testing.T) {
	if got := UniqueName("SRV", map[string]bool{"srv": true, "srv-2": true}); got != "srv-3" {
		t.Fatalf("got %q", got)
	}
	if got := UniqueName("fresh", map[string]bool{"srv": true}); got != "fresh" {
		t.Fatalf("got %q", got)
	}
	// Sanitization applies to the base first.
	if got := UniqueName("My Srv!", map[string]bool{"my-srv": true}); got != "my-srv-2" {
		t.Fatalf("got %q", got)
	}
	// Caller map is not mutated (caller must record). Current behavior pinned.
	taken := map[string]bool{"srv": true}
	if got := UniqueName("srv", taken); got != "srv-2" {
		t.Fatalf("got %q", got)
	}
	if len(taken) != 1 || !taken["srv"] {
		t.Fatalf("taken mutated: %v", taken)
	}
}
