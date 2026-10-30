package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandValueTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		env  map[string]string
		want string
	}{
		{"plain", "hello", nil, "hello"},
		{"empty", "", nil, "hello"[5:]},
		{"single", "Bearer ${OXYP_EDGE_A}", map[string]string{"OXYP_EDGE_A": "v"}, "Bearer v"},
		{"multiple", "${OXYP_EDGE_A}-${OXYP_EDGE_B}", map[string]string{"OXYP_EDGE_A": "1", "OXYP_EDGE_B": "2"}, "1-2"},
		{"adjacent", "${OXYP_EDGE_A}${OXYP_EDGE_B}", map[string]string{"OXYP_EDGE_A": "1", "OXYP_EDGE_B": "2"}, "12"},
		{"repeated", "${OXYP_EDGE_A}/${OXYP_EDGE_A}", map[string]string{"OXYP_EDGE_A": "x"}, "x/x"},
		{"unsetStaysLiteral", "x-${OXYP_EDGE_UNSET_999}-y", nil, "x-${OXYP_EDGE_UNSET_999}-y"},
		// Malformed placeholders never match the regexes, so they stay
		// literal. Current behavior: fail-open for header/url checks too.
		{"emptyVarLiteral", "${}", nil, "${}"},
		{"dashVarLiteral", "${A-B}", nil, "${A-B}"},
		{"unterminatedFileLiteral", "{file:/x", nil, "{file:/x"},
		{"emptyFileRefLiteral", "{file:}", nil, "{file:}"},
		{"unterminatedEnv kept", "a ${OXYP_EDGE_A", map[string]string{"OXYP_EDGE_A": "v"}, "a ${OXYP_EDGE_A"},
		{"emptyValue", "a${OXYP_EDGE_A}b", map[string]string{"OXYP_EDGE_A": ""}, "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := expandValue(tc.in)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("expandValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandValueFileForms(t *testing.T) {
	dir := t.TempDir()
	wf := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"stripLF", "file-secret\n", "Bearer file-secret"},
		{"stripCRLF", "a\r\nb\r\n", "Bearer a\r\nb"},
		{"keepSpaces", "a  ", "Bearer a  "},
		{"stripBlankLines", "a\n\n", "Bearer a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandValue("Bearer {file:" + wf(tc.name, tc.body) + "}")
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestExpandValueFileErrors(t *testing.T) {
	if _, err := expandValue("{file:/nonexistent-xyz}"); err == nil {
		t.Fatal("missing file must error")
	}
	if _, err := expandValue("{file:relative/path}"); err == nil {
		t.Fatal("relative path must error")
	}
}

func TestExpandValueFileReexpansion(t *testing.T) {
	// current behavior: file content is NOT re-scanned for ${VAR} afterwards
	// unless the env lookup happens in the same pass — actually the env pass
	// runs after the file pass, so embedded refs DO expand. Pinned.
	t.Setenv("OXYP_EDGE_RE", "re-expanded")
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("v-${OXYP_EDGE_RE}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := expandValue("{file:" + p + "}")
	if err != nil || got != "v-re-expanded" {
		t.Fatalf("got %q err %v", got, err)
	}
	// Env values containing {file:...} are not re-scanned (single pass). Pinned.
	t.Setenv("OXYP_EDGE_CHAIN", "{file:/nonexistent-xyz}")
	got, err = expandValue("x-${OXYP_EDGE_CHAIN}-y")
	if err != nil || got != "x-{file:/nonexistent-xyz}-y" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestExpandServerSecretsMatrix(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "hdr")
	if err := os.WriteFile(tok, []byte("tok-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OXYP_EDGE_HDR", "hv")
	for _, tc := range []struct {
		name string
		mk   func() *ServerEntry
		want string // "" = no ConfigError
	}{
		{"localEnvUnsetPassthrough", func() *ServerEntry {
			return &ServerEntry{Name: "l", Command: []string{"x"}, Env: map[string]string{"K": "v-${OXYP_EDGE_UNSET_999}"}}
		}, ""},
		{"localEnvFileMissing", func() *ServerEntry {
			return &ServerEntry{Name: "l", Command: []string{"x"}, Env: map[string]string{"K": "{file:/nonexistent-xyz}"}}
		}, "env.K"},
		{"remoteURLUnset", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://${OXYP_EDGE_UNSET_999}/mcp"}
		}, "url has unset"},
		{"remoteURLFile", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/{file:" + tok + "}/mcp"}
		}, ""},
		{"remoteHeaderUnset", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/mcp", Headers: map[string]string{"H": "${OXYP_EDGE_UNSET_999}"}}
		}, "headers.H has unset"},
		{"remoteHeaderFileMissing", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/mcp", Headers: map[string]string{"H": "{file:/nonexistent-xyz}"}}
		}, "headers.H"},
		{"remoteHeaderOK", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/mcp", Headers: map[string]string{"H": "Bearer ${OXYP_EDGE_HDR}+"}}
		}, ""},
		{"malformedPlaceholderPassesCurrent", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/{file:}", Headers: map[string]string{"H": "{file:}"}}
		}, ""}, // current behavior: fail-open, sent literally
		{"oauthClientID", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/mcp", OAuth: &OAuthConfig{ClientID: "${OXYP_EDGE_HDR}"}}
		}, ""},
		{"oauthScopeUnset", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/mcp", OAuth: &OAuthConfig{Scope: "${OXYP_EDGE_UNSET_999}"}}
		}, "oauth.scope has unset"},
		{"oauthTimeoutSkipped", func() *ServerEntry {
			return &ServerEntry{Name: "r", URL: "https://x/mcp", OAuth: &OAuthConfig{Timeout: 30}}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.mk()
			got := expandServerSecrets(s)
			if tc.want == "" && got != "" {
				t.Fatalf("ConfigError = %q", got)
			}
			if tc.want != "" && (got == "" || !contains(got, tc.want)) {
				t.Fatalf("ConfigError = %q, want containing %q", got, tc.want)
			}
			if tc.name == "remoteHeaderOK" && s.Headers["H"] != "Bearer hv+" {
				t.Fatalf("header not expanded: %q", s.Headers["H"])
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
