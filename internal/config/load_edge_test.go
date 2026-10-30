package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustLoad(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	writeTOML(t, dir, body)
	return Load(dir)
}

func TestLoadEmptyDefaults(t *testing.T) {
	cfg, err := mustLoad(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Addr != "127.0.0.1:8379" {
		t.Fatalf("addr = %q", cfg.Daemon.Addr)
	}
	if len(cfg.Servers) != 0 {
		t.Fatalf("servers = %+v", cfg.Servers)
	}
}

func TestLoadFatalShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"missingFile", "", "UNIT-OVERRIDE"},
		{"malformed", "[servers.s\ncommand = [\"x\"]\n", "expected '.' or ']'"},
		{"typeGuard", "[daemon]\noutput_guard = false\n[servers.s]\ncommand = [\"x\"]\n", "output_guard"},
		{"typeCommandString", "[servers.s]\ncommand = \"x\"\n", "command"},
		{"typeCommandBool", "[servers.s]\ncommand = [true]\n", "command"},
		{"dupKey", "[servers.s]\ncommand = [\"x\"]\n[servers.s]\ncommand = [\"y\"]\n", "already been defined"},
		{"badDenyType", "[servers.s]\ncommand = [\"x\"]\n[servers.s.tools]\ndeny = \"*\"\n", "tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "missingFile" {
				_, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
				if err == nil {
					t.Fatal("want error for missing dir")
				}
				return
			}
			_, err := mustLoad(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadUnknownKeysIgnored(t *testing.T) {
	// current behavior: unknown TOML keys are silently dropped (toml.MetaData.Undecoded never checked).
	cfg, err := mustLoad(t, "[daemon]\ntypo_key = 1\naddr = \"127.0.0.1:8379\"\n[servers.s]\ncommand = [\"x\"]\nbogus = true\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["s"].ConfigError != "" {
		t.Fatalf("unknown keys must not flag entry: %q", cfg.Servers["s"].ConfigError)
	}
}

func TestLoadTokenFileForms(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := mustLoad(t, "[daemon]\ntoken = \"{file:"+tok+"}\"\n[servers.s]\ncommand = [\"x\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Token != "s3cret" {
		t.Fatalf("token = %q", cfg.Daemon.Token)
	}
	if _, err := mustLoad(t, "[daemon]\ntoken = \"{file:/nonexistent-tok-xyz}\"\n[servers.s]\ncommand = [\"x\"]\n"); err == nil {
		t.Fatal("missing token file must fail Load")
	}
	// {file:} does not match the fileRefRe, so it stays literal — and with a
	// loopback addr it loads fine. Current behavior: fail-open placeholder.
	cfg, err = mustLoad(t, "[daemon]\ntoken = \"{file:}\"\n[servers.s]\ncommand = [\"x\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Token != "{file:}" {
		t.Fatalf("token = %q", cfg.Daemon.Token)
	}
	// Whitespace token satisfies the non-loopback rule (not trimmed). Current behavior.
	cfg, err = mustLoad(t, "[daemon]\ntoken = \"  padded  \"\naddr = \"0.0.0.0:1\"\n[servers.s]\ncommand = [\"x\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Token != "  padded  " {
		t.Fatalf("token = %q", cfg.Daemon.Token)
	}
}

func TestLoadSoftFailIsolation(t *testing.T) {
	cfg, err := mustLoad(t, "[servers.good]\ncommand = [\"true\"]\n[servers.bad]\ndescription = \"x\"\n[servers.also]\nurl = \"ftp://x/y\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["good"].ConfigError != "" {
		t.Fatalf("good flagged: %q", cfg.Servers["good"].ConfigError)
	}
	if cfg.Servers["bad"].ConfigError == "" || cfg.Servers["also"].ConfigError == "" {
		t.Fatal("bad entries must be flagged without failing Load")
	}
}

func TestDefaultDirBranches(t *testing.T) {
	t.Setenv("OXYP_DIR", "/tmp/oxyp-env")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got := DefaultDir(); got != "/tmp/oxyp-env" {
		t.Fatalf("OXYP_DIR should win: %q", got)
	}
	t.Setenv("OXYP_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-probe")
	if got := DefaultDir(); got != filepath.Join("/tmp/xdg-probe", "oxyp") {
		t.Fatalf("XDG branch = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/home-probe")
	if got := DefaultDir(); got != filepath.Join("/tmp/home-probe", ".oxyp") {
		t.Fatalf("HOME branch = %q", got)
	}
	t.Setenv("HOME", "")
	if got := DefaultDir(); got != ".oxyp" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestLoadAddrDefaultAndConnectorsDir(t *testing.T) {
	cfg, err := mustLoad(t, "[daemon]\naddr = \"\"\n[servers.s]\ncommand = [\"x\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Addr != "127.0.0.1:8379" {
		t.Fatalf("empty addr defaulted to %q", cfg.Daemon.Addr)
	}
	// connectors_dir pointing nowhere is fine.
	cfg, err = mustLoad(t, "[daemon]\nconnectors_dir = \"/nonexistent-probe-dir\"\n[servers.s]\ncommand = [\"x\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers = %+v", cfg.Servers)
	}
}

func TestConnectorNameBranches(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "connectors")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := func(name string) {
		if err := os.WriteFile(filepath.Join(cdir, name), []byte("#!/x\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe("UPPER.SH")     // stem keeps case: UPPER
	exe("multi.tar.gz") // Ext strips only last suffix: multi.tar
	exe("noext")        // no extension: full name
	for _, n := range []string{"skip.txt", ".hidden", "adir"} {
		if err := os.WriteFile(filepath.Join(cdir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(cdir, "realdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTOML(t, dir, "[servers.noext]\ncommand = [\"explicit\"]\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Servers["UPPER"]; !ok {
		t.Fatalf("UPPER.SH not discovered: %v", cfg.Servers)
	}
	if _, ok := cfg.Servers["multi.tar"]; !ok {
		t.Fatalf("multi.tar.gz stem wrong: %v", cfg.Servers)
	}
	if got := cfg.Servers["noext"].Command[0]; got != "explicit" {
		t.Fatalf("explicit entry must win over connector file: %q", got)
	}
	if _, ok := cfg.Servers["skip.txt"]; ok {
		t.Fatal("non-executable must be skipped")
	}
	if _, ok := cfg.Servers["realdir"]; ok {
		t.Fatal("directories must be skipped")
	}
}

func TestTLSFileBranches(t *testing.T) {
	base := t.TempDir()
	ca := filepath.Join(base, "ca.pem")
	cert := filepath.Join(base, "c.pem")
	key := filepath.Join(base, "k.pem")
	for _, p := range []string{ca, cert, key} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := mustLoad(t, "[servers.r]\nurl = \"https://x/mcp\"\n[servers.r.tls]\nca_file = \""+ca+"\"\ncert_file = \""+cert+"\"\nkey_file = \""+key+"\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["r"].ConfigError != "" {
		t.Fatalf("valid TLS flagged: %q", cfg.Servers["r"].ConfigError)
	}
	cfg, err = mustLoad(t, "[servers.r]\nurl = \"https://x/mcp\"\n[servers.r.tls]\nca_file = \"/nonexistent-ca-xyz\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Servers["r"].ConfigError, "tls file unreadable") {
		t.Fatalf("missing CA not flagged: %q", cfg.Servers["r"].ConfigError)
	}
	// chmod 000 files still pass: Stat-only check, not readability. Current behavior.
	if err := os.Chmod(key, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(key, 0o600)
	cfg, err = mustLoad(t, "[servers.r]\nurl = \"https://x/mcp\"\n[servers.r.tls]\ncert_file = \""+cert+"\"\nkey_file = \""+key+"\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.Servers["r"].ConfigError; e != "" {
		t.Fatalf("chmod-000 key flagged %q (Stat-only check passes regardless)", e)
	}
}

func TestOAuthShapeBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		frag string
		want string
	}{
		{"ccFull", "grant_type = \"client_credentials\"\nclient_id = \"i\"\nclient_secret = \"s\"\n", ""},
		{"ccMissing", "grant_type = \"client_credentials\"\n", "needs client_id and client_secret"},
		{"badRedirectAccepted", "redirect_uri = \":::bad\"\n", ""}, // current behavior: malformed redirect_uri accepted
		{"paddedFlowRejected", "flow = \" browser\"\n", "oauth.flow must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := mustLoad(t, "[servers.r]\nurl = \"https://x/mcp\"\n[servers.r.oauth]\n"+tc.frag)
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Servers["r"].ConfigError
			if tc.want == "" && got != "" {
				t.Fatalf("ConfigError = %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("ConfigError = %q, want %q", got, tc.want)
			}
		})
	}
}
