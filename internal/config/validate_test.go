package config

import (
	"strings"
	"testing"
)

// validateServer full branch matrix: every branch pinned against the exact
// current error string. Cases marked "current behavior" document fail-open
// gaps (accepted today) so a future fix visibly breaks the test.

func TestValidateServerBranchMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    ServerEntry
		want string // "" = accepted
	}{
		{"noName", ServerEntry{}, `"name" is required`},
		{"urlAndCommandExclusive", ServerEntry{Name: "s", URL: "https://x/mcp", Command: []string{"x"}}, `server "s": url and command are exclusive`},
		{"badScheme", ServerEntry{Name: "s", URL: "ftp://x/mcp"}, `server "s": url must start with http(s)://`},
		{"bareHost", ServerEntry{Name: "s", URL: "example.com/mcp"}, `server "s": url must start with http(s)://`},
		{"httpsOnlyAcceptedCurrent", ServerEntry{Name: "s", URL: "https://"}, ""}, // current behavior: fail-open, bare "https://" accepted
		{"badTransport", ServerEntry{Name: "s", URL: "https://x/mcp", Transport: "stream"}, `server "s": transport must be "sse" or empty (auto)`},
		{"upperTransportRejected", ServerEntry{Name: "s", URL: "https://x/mcp", Transport: "SSE"}, `server "s": transport must be "sse" or empty (auto)`},
		{"sseOK", ServerEntry{Name: "s", URL: "https://x/mcp", Transport: "sse"}, ""},
		{"emptyHeaderName", ServerEntry{Name: "s", URL: "https://x/mcp", Headers: map[string]string{"": "v"}}, `server "s": bad header ""`},
		{"blankHeaderName", ServerEntry{Name: "s", URL: "https://x/mcp", Headers: map[string]string{"   ": "v"}}, `server "s": bad header "   "`},
		{"headerCRLFValue", ServerEntry{Name: "s", URL: "https://x/mcp", Headers: map[string]string{"A": "x\ny"}}, `server "s": bad header "A"`},
		{"headerCRValue", ServerEntry{Name: "s", URL: "https://x/mcp", Headers: map[string]string{"A": "x\ry"}}, `server "s": bad header "A"`},
		{"interiorSpaceHeaderAcceptedCurrent", ServerEntry{Name: "s", URL: "https://x/mcp", Headers: map[string]string{"a b": "c"}}, ""}, // current behavior: fail-open
		{"cwdRemote", ServerEntry{Name: "s", URL: "https://x/mcp", Cwd: "/tmp"}, `server "s": cwd/inherit_env is local-only`},
		{"inheritEnvRemote", ServerEntry{Name: "s", URL: "https://x/mcp", InheritEnv: new(true)}, `server "s": cwd/inherit_env is local-only`},
		{"remoteEnvAcceptedCurrent", ServerEntry{Name: "s", URL: "https://x/mcp", Env: map[string]string{"A": "b"}}, ""}, // current behavior: remote Env unused but accepted
		{"localTransport", ServerEntry{Name: "s", Command: []string{"x"}, Transport: "sse"}, `server "s": transport is remote-only`},
		{"localHeaders", ServerEntry{Name: "s", Command: []string{"x"}, Headers: map[string]string{"A": "b"}}, `server "s": headers is remote-only`},
		{"localTLS", ServerEntry{Name: "s", Command: []string{"x"}, TLS: &TLSConfig{}}, `server "s": tls is remote-only`},
		{"localAuth", ServerEntry{Name: "s", Command: []string{"x"}, Auth: "oauth"}, `server "s": auth/oauth is remote-only`},
		{"localOAuth", ServerEntry{Name: "s", Command: []string{"x"}, OAuth: &OAuthConfig{}}, `server "s": auth/oauth is remote-only`},
		{"noCommand", ServerEntry{Name: "s"}, `server "s": no command (and no matching connector file)`},
		{"emptyArgv0AcceptedCurrent", ServerEntry{Name: "s", Command: []string{""}}, ""}, // current behavior: fail-open, spawn fails later
		{"badAuth", ServerEntry{Name: "s", URL: "https://x/mcp", Auth: "bearer"}, `server "s": auth must be "oauth" or empty`},
		{"oauthOK", ServerEntry{Name: "s", URL: "https://x/mcp", Auth: "oauth"}, ""},
		{"oauthNegPort", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{CallbackPort: -1}}, `server "s": oauth.callback_port must be >= 0`},
		{"oauthHugePortAcceptedCurrent", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{CallbackPort: 99999}}, ""}, // current behavior: fail-open
		{"oauthBadFlow", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{Flow: " DEVICE "}}, `server "s": oauth.flow must be browser|device|token`},
		{"oauthFlows", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{Flow: "browser"}}, ""},
		{"oauthFlowDevice", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{Flow: "device"}}, ""},
		{"oauthFlowToken", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{Flow: "token"}}, ""},
		{"oauthBadGrant", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{GrantType: "password"}}, `server "s": oauth.grant_type must be "client_credentials" or empty`},
		{"oauthCCMissing", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{GrantType: "client_credentials"}}, `server "s": oauth.grant_type client_credentials needs client_id and client_secret`},
		{"oauthCCHalf", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{GrantType: "client_credentials", ClientID: "i"}}, `server "s": oauth.grant_type client_credentials needs client_id and client_secret`},
		{"oauthCCFull", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{GrantType: "client_credentials", ClientID: "i", ClientSecret: "s"}}, ""},
		{"oauthNegTimeout", ServerEntry{Name: "s", URL: "https://x/mcp", OAuth: &OAuthConfig{Timeout: -3}}, `server "s": oauth.timeout must be >= 0`},
		{"tlsHalfPair", ServerEntry{Name: "s", URL: "https://x/mcp", TLS: &TLSConfig{CertFile: "/x"}}, `server "s": tls cert_file and key_file must both be set`},
		{"tlsMissingFile", ServerEntry{Name: "s", URL: "https://x/mcp", TLS: &TLSConfig{CAFile: "/nonexistent-ca-xyz"}}, `server "s": tls file unreadable: /nonexistent-ca-xyz`},
		{"tlsBlankFileAccepted", ServerEntry{Name: "s", URL: "https://x/mcp", TLS: &TLSConfig{}}, ""},
		{"tlsInsecureNoConflict", ServerEntry{Name: "s", URL: "https://x/mcp", TLS: &TLSConfig{Insecure: true}}, ""}, // current behavior: insecure never conflicts with CA/mTLS
		{"allowEmpty", ServerEntry{Name: "s", Command: []string{"x"}, Tools: &ToolsPolicy{Allow: []string{""}}}, `server "s": tools.allow[0] must not be empty`},
		{"allowBlank", ServerEntry{Name: "s", Command: []string{"x"}, Tools: &ToolsPolicy{Allow: []string{"ok", " "}}}, `server "s": tools.allow[1] must not be empty`},
		{"denyEmpty", ServerEntry{Name: "s", Command: []string{"x"}, Tools: &ToolsPolicy{Deny: []string{"nuke", ""}}}, `server "s": tools.deny[1] must not be empty`},
		{"lifecycleBogus", ServerEntry{Name: "s", Command: []string{"x"}, Lifecycle: "Eager"}, `server "s": lifecycle must be lazy|eager|keep-alive|lazy-keep-alive`},
		{"lifecycleLazyKeepAlive", ServerEntry{Name: "s", Command: []string{"x"}, Lifecycle: "lazy-keep-alive"}, ""},
		{"negIdle", ServerEntry{Name: "s", URL: "https://x/mcp", IdleTimeout: new(-2)}, `server "s": idle_timeout must be >= 0`},
		{"negGuard", ServerEntry{Name: "s", URL: "https://x/mcp", OutputGuard: &OutputGuardConfig{MaxBytes: -1}}, `server "s": output_guard limits must be >= 0`},
		{"negGuardLines", ServerEntry{Name: "s", URL: "https://x/mcp", OutputGuard: &OutputGuardConfig{MaxLines: -2}}, `server "s": output_guard limits must be >= 0`},
		{"traceFile", ServerEntry{Name: "s", Command: []string{"x"}, Trace: &TraceConfig{File: "/tmp/x"}}, `server "s": trace.file is daemon-level only`},
		{"negTrace", ServerEntry{Name: "s", URL: "https://x/mcp", Trace: &TraceConfig{MaxEvents: -1}}, `server "s": trace limits must be >= 0`},
		{"negTraceBytes", ServerEntry{Name: "s", URL: "https://x/mcp", Trace: &TraceConfig{MaxBytes: -9}}, `server "s": trace limits must be >= 0`},
		{"remoteLifecycleAcceptedCurrent", ServerEntry{Name: "s", URL: "https://x/mcp", Lifecycle: "eager"}, ""}, // current behavior: remote lifecycle/idle accepted though sweep is local-only
		{"localHappy", ServerEntry{Name: "s", Command: []string{"true"}}, ""},
		{"remoteHappy", ServerEntry{Name: "s", URL: "https://x/mcp"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateServer(&tc.s); got != tc.want {
				t.Fatalf("validateServer = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateDaemonMatrix(t *testing.T) {
	tokenCases := []struct {
		name string
		d    DaemonConfig
		want string // "" = accepted
	}{
		{"zero", DaemonConfig{}, ""},
		{"emptyAddr", DaemonConfig{Addr: ""}, ""},
		{"explicitLoopback", DaemonConfig{Addr: "127.0.0.1:8379"}, ""},
		{"loopbackIPv6", DaemonConfig{Addr: "[::1]:8379"}, ""},
		{"localhostLower", DaemonConfig{Addr: "localhost:8379"}, ""},
		{"localhostUpperFailsClosedCurrent", DaemonConfig{Addr: "LOCALHOST:8379"}, "daemon.token is required for non-loopback addr"}, // current behavior: case-sensitive
		{"wildcard", DaemonConfig{Addr: "0.0.0.0:8379"}, "daemon.token is required for non-loopback addr"},
		{"barePort", DaemonConfig{Addr: ":8379"}, "daemon.token is required for non-loopback addr"},
		{"ipv6Wildcard", DaemonConfig{Addr: "[::]:8379"}, "daemon.token is required for non-loopback addr"},
		{"lan", DaemonConfig{Addr: "192.168.1.5:8379"}, "daemon.token is required for non-loopback addr"},
		{"unparseable", DaemonConfig{Addr: "not-an-addr"}, "daemon.token is required for non-loopback addr"},
		{"schemePrefixFailsClosedCurrent", DaemonConfig{Addr: "http://127.0.0.1:8379"}, "daemon.token is required for non-loopback addr"}, // current behavior: SplitHostPort host becomes "http"
		{"portNamePassesLoopbackCheck", DaemonConfig{Addr: "127.0.0.1:notaport"}, ""},                                                   // current behavior: syntax never validated
		{"emptyString", DaemonConfig{Addr: ""}, ""},
		{"tokenSatisfies", DaemonConfig{Addr: "0.0.0.0:8379", Token: "x"}, ""},
		{"whitespaceTokenSatisfiesCurrent", DaemonConfig{Addr: "0.0.0.0:1", Token: " "}, ""}, // current behavior: not trimmed
		{"negGuard", DaemonConfig{OutputGuard: &OutputGuardConfig{MaxBytes: -1}}, "daemon.output_guard limits must be >= 0"},
		{"negGuardLines", DaemonConfig{OutputGuard: &OutputGuardConfig{MaxLines: -1}}, "daemon.output_guard limits must be >= 0"},
		{"negTraceBytes", DaemonConfig{Trace: &TraceConfig{MaxBytes: -5}}, "daemon.trace limits must be >= 0"},
		{"negTraceEvents", DaemonConfig{Trace: &TraceConfig{MaxEvents: -5}}, "daemon.trace limits must be >= 0"},
		{"negIdle", DaemonConfig{IdleTimeout: new(-1)}, "daemon.idle_timeout must be >= 0"},
		{"zeroIdle", DaemonConfig{IdleTimeout: new(0)}, ""},
		{"guardOff", DaemonConfig{OutputGuard: &OutputGuardConfig{Enabled: new(false)}}, ""},
	}
	for _, tc := range tokenCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidateDaemon(&tc.d); got != tc.want {
				t.Fatalf("ValidateDaemon = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateDaemonViaLoad(t *testing.T) {
	// Load must hard-fail (not soft-fail) on daemon-wide errors.
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"negGuard", "[daemon]\n[daemon.output_guard]\nmax_bytes = -1\n[servers.s]\ncommand = [\"x\"]\n", "daemon.output_guard limits must be >= 0"},
		{"negTrace", "[daemon]\n[daemon.trace]\nmax_bytes = -5\n[servers.s]\ncommand = [\"x\"]\n", "daemon.trace limits must be >= 0"},
		{"negIdle", "[daemon]\nidle_timeout = -1\n[servers.s]\ncommand = [\"x\"]\n", "daemon.idle_timeout must be >= 0"},
		{"nonLoopbackNoToken", "[daemon]\naddr = \"0.0.0.0:1\"\n[servers.s]\ncommand = [\"x\"]\n", "daemon.token is required for non-loopback addr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTOML(t, dir, tc.body)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
