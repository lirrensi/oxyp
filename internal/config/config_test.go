package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTOML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[daemon]
[servers.time]
description = "clock"
command = ["python3", "-m", "mcp_server_time"]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Addr != "127.0.0.1:8379" {
		t.Fatalf("default addr = %q", cfg.Daemon.Addr)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %+v", cfg.Servers)
	}
	s := cfg.Servers["time"]
	if s.ConfigError != "" {
		t.Fatalf("server flagged: %s", s.ConfigError)
	}
	if s.Name != "time" {
		t.Fatalf("name not filled from key: %q", s.Name)
	}
	if !s.IsEnabled() {
		t.Fatal("nil enabled should default true")
	}
}

func TestLoadSoftFail(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[servers.good]
description = "ok"
command = ["true"]
[servers.bad]
description = "missing command"
[servers.alsobad]
description = "empty pattern"
command = ["true"]
[servers.alsobad.tools]
deny = ["ok_*", " "]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("bad entry must not fail load: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("want 3 servers, got %d", len(cfg.Servers))
	}
	if cfg.Servers["good"].ConfigError != "" {
		t.Fatalf("good server flagged: %s", cfg.Servers["good"].ConfigError)
	}
	if cfg.Servers["bad"].ConfigError == "" {
		t.Fatal("missing command not flagged")
	}
	if cfg.Servers["alsobad"].ConfigError == "" {
		t.Fatal("empty deny pattern not flagged")
	}
}

func TestCwdInheritEnvLocalOnly(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[servers.local]
command = ["true"]
cwd = "/tmp"
inherit_env = false
[servers.remote]
url = "http://127.0.0.1:9/mcp"
cwd = "/tmp"
[servers.remote2]
url = "http://127.0.0.1:9/mcp"
inherit_env = false
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["local"].ConfigError != "" {
		t.Fatalf("local cwd/inherit_env flagged: %s", cfg.Servers["local"].ConfigError)
	}
	if f := false; cfg.Servers["local"].InheritEnv == nil || *cfg.Servers["local"].InheritEnv != f {
		t.Fatal("inherit_env=false must parse as pointer-for-optional")
	}
	for _, n := range []string{"remote", "remote2"} {
		if e := cfg.Servers[n].ConfigError; e == "" || !strings.Contains(e, "local-only") {
			t.Fatalf("%s must be local-only flagged, got %q", n, e)
		}
	}
}

func TestConnectorAutodiscovery(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "connectors")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// executable file → discovered
	if err := os.WriteFile(filepath.Join(cdir, "crm.py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// non-executable → skipped
	if err := os.WriteFile(filepath.Join(cdir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTOML(t, dir, `
[servers.crm]
description = "explicit wins"
command = ["my-crm"]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("want 1 server (explicit wins), got %+v", cfg.Servers)
	}
	if cfg.Servers["crm"].Command[0] != "my-crm" {
		t.Fatalf("explicit entry overwritten: %+v", cfg.Servers["crm"])
	}

	// without explicit entry → discovered by stem
	writeTOML(t, dir, ``)
	cfg, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("discovery failed: %+v", cfg.Servers)
	}
	s, ok := cfg.Servers["crm"]
	if !ok {
		t.Fatalf("discovery failed: %+v", cfg.Servers)
	}
	if s.FromConnector == "" {
		t.Fatal("FromConnector not set")
	}
}

func TestEnabledFlag(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[servers.a]
description = "x"
command = ["true"]
enabled = false
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["a"].IsEnabled() {
		t.Fatal("enabled=false should disable")
	}
}

func TestToolsPolicyNested(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[servers.echo]
command = ["echo"]
[servers.echo.tools]
allow = ["*"]
deny = ["delete_*", "nuke"]
[servers.glean]
url = "https://glean.example.com/mcp"
[servers.glean.tools]
allow = ["search_*"]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Servers["echo"]
	if e.Tools == nil || len(e.Tools.Allow) != 1 || len(e.Tools.Deny) != 2 {
		t.Fatalf("echo tools = %+v", e.Tools)
	}
	g := cfg.Servers["glean"]
	if g.Tools == nil || len(g.Tools.Allow) != 1 || len(g.Tools.Deny) != 0 {
		t.Fatalf("glean tools = %+v", g.Tools)
	}
}

func TestLifecycleKnobs(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[daemon]
idle_timeout = 5
[servers.a]
command = ["true"]
lifecycle = "keep-alive"
[servers.b]
command = ["true"]
idle_timeout = 0
[servers.c]
command = ["true"]
lifecycle = "bogus"
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["c"].ConfigError == "" {
		t.Fatal("bad lifecycle not flagged")
	}
	a := cfg.Servers["a"]
	if a.LifecycleMode() != "keep-alive" || a.WantsIdleStop(cfg.Daemon) {
		t.Fatal("keep-alive must be sweep-exempt")
	}
	if !a.WantsEagerStart() {
		t.Fatal("keep-alive must eager-start")
	}
	b := cfg.Servers["b"]
	if b.EffectiveIdleTimeout(cfg.Daemon) != 0 || b.WantsIdleStop(cfg.Daemon) {
		t.Fatal("idle_timeout=0 must disable sweep")
	}
	// daemon default applies when server unset
	dflt := ServerEntry{Command: []string{"true"}}
	if dflt.EffectiveIdleTimeout(cfg.Daemon) != 5 {
		t.Fatalf("daemon idle default not applied: %d", dflt.EffectiveIdleTimeout(cfg.Daemon))
	}
	if dflt.EffectiveIdleTimeout(DaemonConfig{}) != DefaultIdleTimeoutMin {
		t.Fatal("hard default not applied")
	}
}

func TestDefaultDirPrecedence(t *testing.T) {
	t.Setenv("OXYP_DIR", "/tmp/oxyp-env")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got := DefaultDir(); got != "/tmp/oxyp-env" {
		t.Fatalf("OXYP_DIR should win: %q", got)
	}
}

func TestGuardTraceResolution(t *testing.T) {
	d := DaemonConfig{}
	s := ServerEntry{}
	if s.EffectiveGuard(d) != nil || s.EffectiveTrace(d) != nil {
		t.Fatal("unset must be off")
	}
	d.OutputGuard = &OutputGuardConfig{}
	if g := s.EffectiveGuard(d); g == nil || g.MaxBytes != DefaultGuardMaxBytes || g.MaxLines != DefaultGuardMaxLines {
		t.Fatalf("daemon defaults wrong: %+v", g)
	}
	s.OutputGuard = &OutputGuardConfig{MaxBytes: 10, MaxLines: 5}
	if g := s.EffectiveGuard(d); g.MaxBytes != 10 || g.MaxLines != 5 {
		t.Fatalf("server must win: %+v", g)
	}
	d.Trace = &TraceConfig{Enabled: true}
	if tc := s.EffectiveTrace(d); tc == nil || !tc.Enabled || tc.MaxEvents != DefaultTraceMaxEvents {
		t.Fatalf("trace defaults wrong: %+v", tc)
	}
	s.Trace = &TraceConfig{Enabled: false}
	if tc := s.EffectiveTrace(d); tc.Enabled {
		t.Fatal("server disable must win")
	}
}

func TestGuardEnabledFalseDisables(t *testing.T) {
	off := false
	d := DaemonConfig{OutputGuard: &OutputGuardConfig{MaxBytes: 100, MaxLines: 10}}
	// advertised table syntax must load: output_guard = { enabled = false }
	dir := t.TempDir()
	writeTOML(t, dir, "[daemon]\n[daemon.output_guard]\nenabled = false\n[servers.s]\ncommand = [\"x\"]\n[servers.s.output_guard]\nenabled = false\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("advertised enabled=false must load: %v", err)
	}
	if cfg.Servers["s"].EffectiveGuard(cfg.Daemon) != nil {
		t.Fatal("server enabled=false must disable daemon guard")
	}
	if cfg.Daemon.OutputGuard == nil || (cfg.Daemon.OutputGuard.Enabled != nil && !*cfg.Daemon.OutputGuard.Enabled) == false {
		t.Fatal("daemon enabled=false must parse")
	}
	s := ServerEntry{OutputGuard: &OutputGuardConfig{Enabled: &off}}
	if s.EffectiveGuard(d) != nil {
		t.Fatal("server enabled=false must win over daemon guard")
	}
	dOff := DaemonConfig{OutputGuard: &OutputGuardConfig{Enabled: &off}}
	if (ServerEntry{}).EffectiveGuard(dOff) != nil {
		t.Fatal("daemon enabled=false must be off")
	}
}

func TestNonLoopbackRequiresToken(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8379", ":8379", "[::]:8379", "192.168.1.5:8379"} {
		if msg := ValidateDaemon(&DaemonConfig{Addr: addr}); msg == "" {
			t.Fatalf("addr %q with empty token must fail", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8379", "[::1]:8379", "localhost:8379"} {
		if msg := ValidateDaemon(&DaemonConfig{Addr: addr}); msg != "" {
			t.Fatalf("loopback addr %q must pass: %s", addr, msg)
		}
	}
	// token present + non-loopback passes
	if msg := ValidateDaemon(&DaemonConfig{Addr: "0.0.0.0:8379", Token: "x"}); msg != "" {
		t.Fatalf("token must satisfy non-loopback: %s", msg)
	}
}

func TestDaemonTokenExpansionFailClosed(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, "[daemon]\ntoken = \"${OXYP_TEST_UNSET_999}\"\n[servers.s]\ncommand = [\"x\"]\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("leftover ${...} daemon token must fail Load")
	}
	t.Setenv("OXYP_TEST_TOKEN_X", "s3cret")
	dir2 := t.TempDir()
	writeTOML(t, dir2, "[daemon]\ntoken = \"${OXYP_TEST_TOKEN_X}\"\n[servers.s]\ncommand = [\"x\"]\n")
	cfg, err := Load(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Token != "s3cret" {
		t.Fatalf("token not expanded: %q", cfg.Daemon.Token)
	}
}

func TestOAuthExpansionFailClosed(t *testing.T) {
	s := &ServerEntry{Name: "r", URL: "https://x/mcp",
		OAuth: &OAuthConfig{ClientSecret: "${OXYP_TEST_UNSET_999}"}}
	if msg := expandServerSecrets(s); msg == "" {
		t.Fatal("want fail-closed on unset oauth var")
	}
	t.Setenv("OXYP_TEST_OAUTH_X", "v")
	ok := &ServerEntry{Name: "r", URL: "https://x/mcp",
		OAuth: &OAuthConfig{ClientID: "${OXYP_TEST_OAUTH_X}", Scope: "a"}}
	if msg := expandServerSecrets(ok); msg != "" {
		t.Fatalf("set oauth vars must expand: %s", msg)
	}
	if ok.OAuth.ClientID != "v" {
		t.Fatalf("oauth client_id not expanded: %q", ok.OAuth.ClientID)
	}
}

func TestPerServerTraceFileRejected(t *testing.T) {
	s := &ServerEntry{Name: "s", Command: []string{"x"}, Trace: &TraceConfig{Enabled: true, File: "/tmp/x.jsonl"}}
	if msg := validateServer(s); !strings.Contains(msg, "daemon-level only") {
		t.Fatalf("want trace.file rejection, got %q", msg)
	}
}
