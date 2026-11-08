package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerLoadAndDefaults(t *testing.T) {
	dir := t.TempDir()
	toml := `[peers.wsl]
url = "http://127.0.0.1:8380"
token = "sekret"
include = ["linear"]
[peers.wsl.servers.github]
priority = -10
`
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Peers["wsl"]
	if p.Name != "wsl" || p.ConfigError != "" {
		t.Fatalf("peer = %+v", p)
	}
	if p.ModeOr() != PeerModeMerge {
		t.Fatalf("default mode = %q", p.ModeOr())
	}
	if p.ServerPriority("github") != -10 || p.ServerPriority("linear") != 0 {
		t.Fatalf("priority = %d/%d", p.ServerPriority("github"), p.ServerPriority("linear"))
	}
	full, leaf := 3, 7
	pp := PeerEntry{Priority: 9, Servers: map[string]PeerServer{
		"c:gamma": {Priority: &full}, "gamma": {Priority: &leaf}}}
	if pp.ServerPriority("c:gamma") != 3 || pp.ServerPriority("x:gamma") != 7 || pp.ServerPriority("other") != 9 {
		t.Fatal("full-key must beat leaf beats default")
	}
	if Qualify("wsl", "linear") != "wsl:linear" {
		t.Fatal("Qualify broken")
	}
}

func TestPeerValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PeerEntry)
		want string
	}{
		{"ok", func(p *PeerEntry) {}, ""},
		{"noURL", func(p *PeerEntry) { p.URL = "" }, "url is required"},
		{"badScheme", func(p *PeerEntry) { p.URL = "ftp://x" }, "url must start with http(s)://"},
		{"badMode", func(p *PeerEntry) { p.Mode = "mesh" }, "mode must be merge|namespace"},
		{"namespaceOK", func(p *PeerEntry) { p.Mode = "namespace" }, ""},
		{"colonName", func(p *PeerEntry) { p.Name = "a:b" }, "must not contain ':'"},
		{"localReserved", func(p *PeerEntry) { p.Name = "local" }, "reserved"},
		{"emptyInclude", func(p *PeerEntry) { p.Include = []string{""} }, "include[0]"},
		{"emptyOverrideName", func(p *PeerEntry) { p.Servers = map[string]PeerServer{"": {}} }, "empty server name"},
		{"routeOverrideOK", func(p *PeerEntry) { pr := 1; p.Servers = map[string]PeerServer{"a:b": {Priority: &pr}} }, ""},
		{"emptyBlock", func(p *PeerEntry) { p.Servers = map[string]PeerServer{"g": {}} }, "is empty"},
		{"toolsOnlyOK", func(p *PeerEntry) { p.Servers = map[string]PeerServer{"g": {Tools: &ToolsPolicy{Deny: []string{"x"}}}} }, ""},
		{"badDeny", func(p *PeerEntry) { p.Servers = map[string]PeerServer{"g": {Tools: &ToolsPolicy{Deny: []string{""}}}} }, "tools.deny[0]"},
		{"badParam", func(p *PeerEntry) { p.Servers = map[string]PeerServer{"g": {Params: map[string][]string{"t": {""}}}} }, "params.t[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &PeerEntry{Name: "w", URL: "http://127.0.0.1:9"}
			tc.mut(p)
			got := validatePeer(p)
			if tc.want == "" && got != "" {
				t.Fatalf("want ok, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q in %q", tc.want, got)
			}
		})
	}
}

func TestServerColonRejected(t *testing.T) {
	s := &ServerEntry{Name: "w:github", Command: []string{"x"}}
	if msg := validateServer(s); !strings.Contains(msg, "reserved for peer:server") {
		t.Fatalf("colon name must be rejected, got %q", msg)
	}
}

func TestPeerSecretsFailClosed(t *testing.T) {
	p := &PeerEntry{Name: "w", URL: "http://${OXYP_PEER_UNSET_999}/x"}
	if msg := expandPeerSecrets(p); !strings.Contains(msg, "unset") {
		t.Fatalf("unset url var must fail closed, got %q", msg)
	}
	p2 := &PeerEntry{Name: "w", URL: "http://127.0.0.1:9", Token: "${OXYP_PEER_UNSET_999}"}
	if msg := expandPeerSecrets(p2); !strings.Contains(msg, "unset") {
		t.Fatalf("unset token var must fail closed, got %q", msg)
	}
	t.Setenv("OXYP_PEER_TOK", "v")
	p3 := &PeerEntry{Name: "w", URL: "http://127.0.0.1:9", Token: "x-${OXYP_PEER_TOK}"}
	if msg := expandPeerSecrets(p3); msg != "" || p3.Token != "x-v" {
		t.Fatalf("set token must expand: %q %+v", msg, p3)
	}
}

func TestPeerDeepCopy(t *testing.T) {
	pr := 5
	en := true
	p := PeerEntry{Name: "w", Include: []string{"a"}, Servers: map[string]PeerServer{"g": {Priority: &pr}},
		TLS: &TLSConfig{CAFile: "c"}, Enabled: &en}
	c := p.DeepCopy()
	c.Include[0] = "z"
	*c.Servers["g"].Priority = 9
	if p.Include[0] != "a" || *p.Servers["g"].Priority != 5 {
		t.Fatal("deep copy aliased")
	}
}

func TestPeerSharingValidation(t *testing.T) {
	cases := []struct {
		name string
		ps   *PeerSharingConfig
		want string
	}{
		{"nilOK", nil, ""},
		{"emptyOK", &PeerSharingConfig{}, ""},
		{"hideOK", &PeerSharingConfig{Mode: "hide"}, ""},
		{"showOK", &PeerSharingConfig{Mode: "show", Allow: []string{"a*"}, Deny: []string{"*secret"}}, ""},
		{"badMode", &PeerSharingConfig{Mode: "mesh"}, "hide|show"},
		{"emptyAllow", &PeerSharingConfig{Allow: []string{""}}, "allow[0]"},
		{"emptyDeny", &PeerSharingConfig{Deny: []string{" "}}, "deny[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateDaemon(&DaemonConfig{PeerSharing: tc.ps})
			if tc.want == "" && got != "" {
				t.Fatalf("want ok, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q in %q", tc.want, got)
			}
		})
	}
	if (&PeerSharingConfig{}).ModeOr() != PeerSharingHide {
		t.Fatal("default mode must be hide")
	}
	var nilPS *PeerSharingConfig
	if nilPS.ModeOr() != PeerSharingHide || nilPS.DeepCopy() != nil {
		t.Fatal("nil sharing must default hide + copy nil")
	}
}

func TestPeerSharingLoad(t *testing.T) {
	dir := t.TempDir()
	toml := `[daemon.peer_sharing]
mode = "show"
allow = ["pub*"]
deny = ["*secret"]
`
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ps := cfg.Daemon.PeerSharing
	if ps == nil || ps.ModeOr() != "show" || len(ps.Allow) != 1 || len(ps.Deny) != 1 {
		t.Fatalf("sharing = %+v", ps)
	}
	dc := cfg.Daemon.DeepCopy()
	dc.PeerSharing.Allow[0] = "z"
	if cfg.Daemon.PeerSharing.Allow[0] != "pub*" {
		t.Fatal("DeepCopy aliased sharing globs")
	}
}

func TestDaemonNameValidation(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"", ""}, {"central", ""}, {"wsl-box", ""},
		{"a:b", "must not contain"}, {"local", "reserved"},
	} {
		got := ValidateDaemon(&DaemonConfig{Name: tc.name})
		if tc.want == "" && got != "" {
			t.Fatalf("%q: want ok, got %q", tc.name, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Fatalf("%q: want %q in %q", tc.name, tc.want, got)
		}
	}
}
