package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"context"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/importcfg"
)

// withStdin swaps os.Stdin for fn.
func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	if input != "" {
		_, _ = w.WriteString(input)
	}
	_ = w.Close()
	defer func() { os.Stdin = old }()
	fn()
}

func TestConfirmWriteVariants(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  y  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"maybe\n", false},
	} {
		var got bool
		_, _ = captureBoth(t, func() {
			withStdin(t, tc.in, func() { got = confirmWrite("/tmp/x") })
		})
		if got != tc.want {
			t.Errorf("input %q: got %v want %v", tc.in, got, tc.want)
		}
	}
	// EOF counts as no.
	var gotEOF bool
	_, _ = captureBoth(t, func() {
		withStdin(t, "", func() { gotEOF = confirmWrite("/tmp/x") })
	})
	if gotEOF {
		t.Error("EOF must be false")
	}
	// No trailing newline: ReadString returns err -> false (current behavior pin).
	var gotNoNL bool
	_, _ = captureBoth(t, func() {
		withStdin(t, "y", func() { gotNoNL = confirmWrite("/tmp/x") })
	})
	if gotNoNL {
		t.Error("current behavior: 'y' without newline is false (ReadString err)")
	}
}

func TestLoadOrEmptyVariants(t *testing.T) {
	// missing -> empty non-nil
	cfg, err := loadOrEmpty(t.TempDir())
	if err != nil || cfg == nil || cfg.Servers == nil {
		t.Fatalf("missing: %v %+v", cfg, err)
	}
	// valid
	dir := t.TempDir()
	writeTOML(t, dir, "[servers.a]\ncommand = [\"echo\"]\n")
	cfg, err = loadOrEmpty(dir)
	if err != nil || cfg.Servers["a"].Command[0] != "echo" {
		t.Fatalf("valid: %v %+v", cfg, err)
	}
	// corrupt
	dir2 := t.TempDir()
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(importcfg.ConfigPath(dir2), []byte("[[["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrEmpty(dir2); err == nil {
		t.Fatal("corrupt must error")
	}
	// path is a directory
	dir3 := t.TempDir()
	if err := os.MkdirAll(importcfg.ConfigPath(dir3), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrEmpty(dir3); err == nil {
		t.Fatal("directory path must error")
	}
}

func TestTakenNamesUniqueNameCase(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"Foo": {}, "foo-2": {},
	}}
	taken := takenNames(cfg)
	if !taken["Foo"] {
		t.Fatal("taken must include Foo")
	}
	if got := importcfg.UniqueName("foo", taken); got != "foo-3" {
		t.Fatalf("got %q want foo-3", got)
	}
	if got := importcfg.UniqueName("bar", taken); got != "bar" {
		t.Fatalf("got %q want bar", got)
	}
}

func mcpMPTS(t *testing.T) *httptest.Server {
	t.Helper()
	msrv := mcpserver.NewMCPServer("add", "1.0")
	msrv.AddTool(mcp.Tool{Name: "ping", Description: "p"}, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "ok"}}}, nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(msrv))
	t.Cleanup(ts.Close)
	return ts
}

func TestAddWriteAgainstHTTPSCURL(t *testing.T) {
	ts := mcpMPTS(t)
	dir := t.TempDir()
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{ts.URL + "/mcp", "--write"})
	_, errOut := captureBoth(t, func() {
		if err := c.Execute(); err != nil {
			t.Errorf("add: %v", err)
		}
	})
	_ = errOut
	data, err := os.ReadFile(importcfg.ConfigPath(dir))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	st, _ := os.Stat(importcfg.ConfigPath(dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o want 600", st.Mode().Perm())
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers: %+v", cfg.Servers)
	}
	for name, e := range cfg.Servers {
		if e.URL != ts.URL+"/mcp" {
			t.Fatalf("url = %q", e.URL)
		}
		if !strings.HasPrefix(dataString(data), "[servers."+name+"]") && !strings.Contains(dataString(data), name) {
			t.Fatalf("block missing name %q: %q", name, data)
		}
	}
}

func dataString(b []byte) string { return string(b) }

func TestAddSanitizeCollisionPortDrop(t *testing.T) {
	if got := importcfg.SanitizeName("Foo Bar!"); got != "foo-bar" {
		t.Fatalf("sanitize = %q", got)
	}
	if n, err := importcfg.NameFromURL("https://example.com:8443/mcp"); err != nil || n != "example-com" {
		t.Fatalf("port must drop: %q %v", n, err)
	}
	ts := mcpMPTS(t)
	// Deriving the same name twice: UniqueName picks the -2 suffix without touching disk.
	dir := t.TempDir()
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{ts.URL + "/mcp", "--write"})
	if err := c.Execute(); err != nil {
		t.Fatalf("add: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("servers: %v", cfg.Servers)
	}
	var first string
	for name := range cfg.Servers {
		first = name
	}
	// Second add of the identical URL would collide: UniqueName resolves it in memory.
	if got := importcfg.UniqueName(first, takenNames(cfg)); got != first+"-2" {
		t.Fatalf("collision: got %q want %s-2", got, first)
	}
	// Explicit collision with an existing local entry.
	dir2 := t.TempDir()
	writeTOML(t, dir2, "[servers.demo]\ncommand = [\"echo\"]\n")
	c2 := addCmd()
	withDirFlag(t, dir2)
	c2.SetArgs([]string{ts.URL + "/mcp", "--write", "--name", "demo"})
	if err := c2.Execute(); err != nil {
		t.Fatalf("add: %v", err)
	}
	cfg2, err := config.Load(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg2.Servers["demo-2"]; !ok {
		t.Fatalf("want demo-2, got %v", cfg2.Servers)
	}
}

func TestAddProbeFailureNoWrite(t *testing.T) {
	// 500 MCP endpoint: also no write.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("nope"))
	}))
	defer bad.Close()
	dir2 := t.TempDir()
	c2 := addCmd()
	withDirFlag(t, dir2)
	c2.SetArgs([]string{bad.URL, "--write"})
	if err := c2.Execute(); err == nil {
		t.Fatal("500 probe must error")
	}
	if _, err := os.Stat(importcfg.ConfigPath(dir2)); !os.IsNotExist(err) {
		t.Fatal("no write on 500")
	}
	// invalid MCP (HTML page): no write.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>hi</html>"))
	}))
	defer html.Close()
	dir3 := t.TempDir()
	c3 := addCmd()
	withDirFlag(t, dir3)
	c3.SetArgs([]string{html.URL, "--write"})
	if err := c3.Execute(); err == nil {
		t.Fatal("html probe must error")
	}
	if _, err := os.Stat(importcfg.ConfigPath(dir3)); !os.IsNotExist(err) {
		t.Fatal("no write on html")
	}
}

func TestAddProbeRefusedNoWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("refused-port probe takes ~40s (initTO+reqTO+probe retries)")
	}
	dir := t.TempDir()
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{"http://127.0.0.1:1/mcp", "--write"})
	if err := c.Execute(); err == nil {
		t.Fatal("probe failure must error")
	}
	if _, err := os.Stat(importcfg.ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatal("no file must be written on probe failure")
	}
}
func TestAddInteractiveNoWrite(t *testing.T) {
	ts := mcpMPTS(t)
	dir := t.TempDir()
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{ts.URL + "/mcp"})
	_, errOut := captureBoth(t, func() {
		withStdin(t, "n\n", func() {
			if err := c.Execute(); err != nil {
				t.Errorf("interactive n must return nil: %v", err)
			}
		})
	})
	if !strings.Contains(errOut, "not written") {
		t.Fatalf("stderr = %q", errOut)
	}
	if _, err := os.Stat(importcfg.ConfigPath(dir)); !os.IsNotExist(err) {
		t.Fatal("interactive n must not write")
	}
}

func TestAddExisting0644Tightened(t *testing.T) {
	ts := mcpMPTS(t)
	dir := t.TempDir()
	writeTOML(t, dir, "[servers.old]\ncommand = [\"echo\"]\n")
	if err := os.Chmod(importcfg.ConfigPath(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{ts.URL + "/mcp", "--write"})
	var errOut string
	_, errOut = captureBoth(t, func() {
		if err := c.Execute(); err != nil {
			t.Errorf("add: %v", err)
		}
	})
	st, _ := os.Stat(importcfg.ConfigPath(dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o want 600", st.Mode().Perm())
	}
	if !strings.Contains(errOut, "tightened") {
		t.Fatalf("want tighten note, got %q", errOut)
	}
	data, _ := os.ReadFile(importcfg.ConfigPath(dir))
	if !strings.Contains(string(data), "old") {
		t.Fatal("existing content must be preserved")
	}
}

func TestAddOAuth401NeedsAuthCurrent(t *testing.T) {
	// 401 with resource_metadata -> needs-auth: entry still written with --write (current behavior).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{srv.URL + "/mcp", "--write"})
	var out, errOut string
	out, errOut = captureBoth(t, func() {
		if err := c.Execute(); err != nil {
			t.Errorf("needs-auth add must not error with --write: %v", err)
		}
	})
	if !strings.Contains(out, "needs-auth") {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "oxyp login") {
		t.Fatalf("stderr = %q", errOut)
	}
	data, err := os.ReadFile(importcfg.ConfigPath(dir))
	if err != nil {
		t.Fatalf("entry must still be written (current behavior): %v", err)
	}
	_ = data
}

func TestAddBadNameSchemeBypass(t *testing.T) {
	// explicit --name bypasses URL scheme validation in add (probe fails, error, no write).
	dir := t.TempDir()
	c := addCmd()
	withDirFlag(t, dir)
	c.SetArgs([]string{"ftp://example.com/mcp", "--write", "--name", "x"})
	if err := c.Execute(); err == nil {
		t.Fatal("ftp probe must error")
	}
	if _, err := os.Stat(importcfg.ConfigPath(dir)); !os.IsNotExist(err) {
		b, _ := os.ReadFile(filepath.Join(dir, "oxyp.toml"))
		t.Fatalf("no write expected, got %q", b)
	}
}

func TestAddDuplicateIdenticalCurrent(t *testing.T) {
	// Appending a second block must keep config.Load working (regression:
	// duplicate [servers] super-table header used to break reload).
	dir := t.TempDir()
	e := config.ServerEntry{Description: "d", URL: "http://127.0.0.1:1/mcp"}
	b1, err := importcfg.RenderBlock("one", e, nil)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := importcfg.RenderBlock("two", e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importcfg.AppendBlocks(dir, b1); err != nil {
		t.Fatal(err)
	}
	if _, err := importcfg.AppendBlocks(dir, b2); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("double append must stay loadable: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("want 2 servers, got %d", len(cfg.Servers))
	}
}

func TestAddNameFromURLPortDrop(t *testing.T) {
	n, err := importcfg.NameFromURL("https://example.com:8443/mcp")
	if err != nil || n != "example-com" {
		t.Fatalf("NameFromURL = %q %v", n, err)
	}
	_ = io.Discard
}
