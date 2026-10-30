package daemon

// Tests for HTTP-side resources/prompts/instructions (#11), endpoint probe
// hints (#14), and per-server cwd/inherit_env (#15). Live tests against the
// python zoo live in oxyp/zoo; these are fast unit tests for the pure
// helpers plus validation-adjacent behavior.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
)

// startHintServer serves one fixed body/status for probe tests.
func startHintServer(t *testing.T, ctype, body string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestChildEnvInheritsByDefault(t *testing.T) {
	os.Setenv("OXYP_TEST_SENTINEL", "present")
	defer os.Unsetenv("OXYP_TEST_SENTINEL")
	env := childEnv(config.ServerEntry{})
	found := false
	for _, kv := range env {
		if kv == "OXYP_TEST_SENTINEL=present" {
			found = true
		}
	}
	if !found {
		t.Fatal("default childEnv must inherit daemon env")
	}
}

func TestChildEnvCleanWhenDisabled(t *testing.T) {
	f := false
	os.Setenv("OXYP_TEST_SENTINEL", "present")
	defer os.Unsetenv("OXYP_TEST_SENTINEL")
	env := childEnv(config.ServerEntry{
		InheritEnv: &f,
		Env:        map[string]string{"EXPLICIT": "yes"},
	})
	for _, kv := range env {
		if strings.HasPrefix(kv, "OXYP_TEST_SENTINEL=") {
			t.Fatalf("clean env must not inherit daemon env: %v", env)
		}
		if kv == "EXPLICIT=yes" {
			goto ok
		}
	}
	t.Fatalf("explicit overlay missing in clean env: %v", env)
ok:
	for _, kv := range env {
		k := strings.SplitN(kv, "=", 2)[0]
		switch k {
		case "PATH", "HOME", "SYSTEMROOT", "EXPLICIT", "PYTHONUNBUFFERED":
		default:
			t.Fatalf("clean env has unexpected key %q: %v", k, env)
		}
	}
}

func TestChildEnvOverlayWins(t *testing.T) {
	os.Setenv("OXYP_OVERLAY_TEST", "daemon")
	defer os.Unsetenv("OXYP_OVERLAY_TEST")
	env := childEnv(config.ServerEntry{Env: map[string]string{"OXYP_OVERLAY_TEST": "child"}})
	found := false
	for _, kv := range env {
		if kv == "OXYP_OVERLAY_TEST=child" {
			found = true
		}
	}
	if !found {
		t.Fatalf("explicit env must overlay inherited: %v", env)
	}
}

func TestCommandFuncNilByDefault(t *testing.T) {
	if fn := commandFuncFor(config.ServerEntry{}); fn != nil {
		t.Fatal("default entry must yield nil CommandFunc (mcp-go default)")
	}
}

func TestCommandFuncNeededForCleanEnv(t *testing.T) {
	f := false
	if fn := commandFuncFor(config.ServerEntry{InheritEnv: &f}); fn == nil {
		t.Fatal("inherit_env=false must yield a CommandFunc (else mcp-go re-inherits)")
	}
}

func TestCommandFuncAppliesCwd(t *testing.T) {
	dir := t.TempDir()
	fn := commandFuncFor(config.ServerEntry{Cwd: dir})
	if fn == nil {
		t.Fatal("cwd must yield a CommandFunc")
	}
	cmd, err := fn(context.Background(), "true", []string{"A=1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != dir {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, dir)
	}
}

func TestCommandFuncCleanEnvExact(t *testing.T) {
	f := false
	fn := commandFuncFor(config.ServerEntry{InheritEnv: &f})
	cmd, err := fn(context.Background(), "true", []string{"A=1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Env) != 1 || cmd.Env[0] != "A=1" {
		t.Fatalf("clean func must set cmd.Env exactly, got %v", cmd.Env)
	}
	if cmd.Dir != "" {
		t.Fatalf("no cwd must leave Dir empty, got %q", cmd.Dir)
	}
}

func TestProbeRefusedHint(t *testing.T) {
	// A just-closed localhost port reliably refuses (RST).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	hint := probeRemote(config.ServerEntry{
		URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/mcp",
	}, 5*time.Second)
	if !strings.Contains(hint, "refused") {
		t.Fatalf("want refused hint, got %q", hint)
	}
}

func TestProbeBadHostHint(t *testing.T) {
	hint := probeRemote(config.ServerEntry{URL: "http://nonexistent.invalid/mcp"}, 5*time.Second)
	if hint == "" {
		t.Skip("no DNS failure observed (offline resolver may differ)")
	}
	if !strings.Contains(hint, "host") && !strings.Contains(hint, "DNS") {
		t.Fatalf("want host hint, got %q", hint)
	}
}

func TestProbeHTMLHint(t *testing.T) {
	srv := startHintServer(t, "text/html", "<html>hello</html>", 200)
	hint := probeRemote(config.ServerEntry{URL: srv}, 5*time.Second)
	if !strings.Contains(hint, "HTML") {
		t.Fatalf("want HTML hint, got %q", hint)
	}
}

func TestProbeStatusHints(t *testing.T) {
	srv404 := startHintServer(t, "application/json", "{}", 404)
	if hint := probeRemote(config.ServerEntry{URL: srv404}, 5*time.Second); !strings.Contains(hint, "404") {
		t.Fatalf("want 404 hint, got %q", hint)
	}
	srv500 := startHintServer(t, "application/json", "{}", 500)
	if hint := probeRemote(config.ServerEntry{URL: srv500}, 5*time.Second); !strings.Contains(hint, "500") {
		t.Fatalf("want 500 hint, got %q", hint)
	}
}

func TestProbeHintSurfacesInHealth(t *testing.T) {
	// A just-closed localhost port reliably refuses (RST). Port 1 and
	// friends may filter/timeout in sandboxed environments instead.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"dead": {Command: nil, URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/mcp"},
	}}, testLog())
	defer d.Close()
	// Force a connect failure through the public path.
	if _, err := d.ListTools(context.Background(), "dead"); err == nil {
		t.Fatal("expected connect failure")
	}
	h := d.HealthOf("dead")
	if h == nil || h.Healthy || !strings.Contains(h.Error, "refused") {
		t.Fatalf("health must carry the probe hint: %+v", h)
	}
}

func TestReadResourceRejectsDotDot(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	if _, err := d.ReadResource(context.Background(), "s", "zoo://a/../b"); err == nil ||
		!strings.Contains(err.Error(), "..") {
		t.Fatalf("want .. rejection, got %v", err)
	}
}

func TestCwdValidation(t *testing.T) {
	// Validation lives in config.Load, not daemon.New: assert the local
	// entry is kept verbatim here; config-level rejection is covered in
	// config package tests.
	dir := t.TempDir()
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"local": {Command: []string{"true"}, Cwd: dir},
	}}, testLog())
	defer d.Close()
	if d.Entry("local") == nil || d.Entry("local").Cwd != dir {
		t.Fatal("local cwd entry must be kept")
	}
}

func TestInvalidateResourcesClearsCaches(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	d.mu.RLock()
	m := d.servers["s"]
	d.mu.RUnlock()
	m.resMu.Lock()
	m.instructions = "cached"
	m.resMu.Unlock()
	d.InvalidateResources("s")
	m.resMu.RLock()
	defer m.resMu.RUnlock()
	if m.res != nil || m.prompts != nil {
		t.Fatal("invalidate must clear res/prompts")
	}
	if m.instructions == "" {
		t.Fatal("invalidate keeps init-time instructions")
	}
}

// TestSpawnWiringComposes asserts the spawn wiring pieces compose:
// clean env + cwd func apply together.
func TestSpawnWiringComposes(t *testing.T) {
	f := false
	cfg := config.ServerEntry{Cwd: t.TempDir(), InheritEnv: &f}
	if commandFuncFor(cfg) == nil {
		t.Fatal("cwd must produce a CommandFunc")
	}
	if got := len(childEnv(cfg)); got == 0 {
		t.Fatal("clean env must at least carry overlays/PATH-ish base")
	}
}
