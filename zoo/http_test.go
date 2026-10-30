package zoo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oxyp/internal/config"
	"oxyp/internal/daemon"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startZooHTTP(t *testing.T, mode string, port int, extra ...string) *exec.Cmd {
	t.Helper()
	args := append([]string{filepath.Join(zooPath(t), "http", "zoo_http.py"),
		"--mode", mode, "--port", fmt.Sprint(port)}, extra...)
	cmd := exec.Command("python3", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// Wait for the ZOO to answer, not just TCP-accept: on WSL2 a bare dial
	// to a just-closed listener succeeds (ghost accept), so probe HTTP.
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for range 50 {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/sse", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return cmd
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("zoo-http %s never listened on %d", mode, port)
	return nil
}

func remoteCfg(name, url string, headers map[string]string) *config.Config {
	return &config.Config{Servers: map[string]config.ServerEntry{
		name: {URL: url, Headers: headers},
	}}
}

func TestHTTPZoo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("clean", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "clean", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 2 {
			t.Fatalf("list: tools=%d err=%v", len(tools), err)
		}
		res, err := d.CallTool(ctx, "h", "http_echo", map[string]any{"msg": "yo"})
		if err != nil || res.IsError {
			t.Fatalf("call: %v %+v", err, res)
		}
	})

	t.Run("bearer-ok", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "bearer", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			map[string]string{"Authorization": "Bearer s3cret"}), testLog())
		defer d.Close()
		if _, err := d.ListTools(ctx, "h"); err != nil {
			t.Fatalf("list with bearer: %v", err)
		}
	})

	t.Run("bearer-missing-401", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "bearer", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		_, err := d.ListTools(ctx, "h")
		if err == nil {
			t.Fatal("want 401 failure, got success")
		}
		t.Logf("failed as expected: %v", err)
	})

	t.Run("custom-header", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "custom-header", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			map[string]string{"X-Tenant": "acme"}), testLog())
		defer d.Close()
		if _, err := d.ListTools(ctx, "h"); err != nil {
			t.Fatalf("list with tenant header: %v", err)
		}
	})

	t.Run("sse-stream-response", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "sse-stream", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 2 {
			t.Fatalf("sse-stream list: tools=%d err=%v", len(tools), err)
		}
	})

	t.Run("same-origin-redirect", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "redirect", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		if _, err := d.ListTools(ctx, "h"); err != nil {
			t.Fatalf("redirect list: %v", err)
		}
	})

	t.Run("broken-json", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "broken-json", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		_, err := d.ListTools(ctx, "h")
		if err == nil {
			t.Fatal("want broken-json failure, got success")
		}
		t.Logf("failed as expected: %v", err)
	})

	t.Run("wrong-content-type", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "wrong-ct", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		t.Logf("wrong-ct: tools=%d err=%v", len(tools), err)
	})

	t.Run("paginated-150", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "paginated", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 150 {
			t.Fatalf("paginated: tools=%d err=%v", len(tools), err)
		}
	})

	t.Run("slow-15s-times-out", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "slow", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		start := time.Now()
		_, err := d.ListTools(ctx, "h")
		if err == nil {
			t.Fatal("want slow timeout, got success")
		}
		t.Logf("timed out after %v: %v", time.Since(start), err)
	})

	t.Run("connection-refused", func(t *testing.T) {
		port := freePort(t) // nothing listening
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		_, err := d.ListTools(ctx, "h")
		if err == nil {
			t.Fatal("want refused failure, got success")
		}
		t.Logf("failed as expected: %v", err)
		if h := d.HealthOf("h"); h == nil || h.Healthy || h.Error == "" {
			t.Fatalf("health not recorded: %+v", h)
		}
	})

	t.Run("cold-remote-stays-cold", func(t *testing.T) {
		// daemon with remote entry must not dial until first use
		d := daemon.New(remoteCfg("h", "http://127.0.0.1:1/mcp", nil), testLog())
		defer d.Close()
		if h := d.HealthOf("h"); h != nil {
			t.Fatalf("remote should have no health before use: %+v", h)
		}
	})

	t.Run("session-id-required", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "session", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 2 {
			t.Fatalf("session list: tools=%d err=%v", len(tools), err)
		}
	})

	t.Run("session-expire-reinits-once", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "session-expire", port)
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil), testLog())
		defer d.Close()
		if _, err := d.ListTools(ctx, "h"); err != nil {
			t.Fatalf("first list: %v", err)
		}
		d.InvalidateTools("h")
		// second list hits expired session → daemon re-inits → succeeds
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 2 {
			t.Fatalf("post-expiry list: tools=%d err=%v", len(tools), err)
		}
	})

	t.Run("legacy-sse-auto-fallback", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "legacy-sse", port)
		// point at /sse: streamable POST 404s → ErrLegacySSEServer → SSE dial
		d := daemon.New(remoteCfg("h", fmt.Sprintf("http://127.0.0.1:%d/sse", port), nil), testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 2 {
			t.Fatalf("sse fallback list: tools=%d err=%v", len(tools), err)
		}
		res, err := d.CallTool(ctx, "h", "http_echo", map[string]any{"msg": "sse"})
		if err != nil || res.IsError {
			t.Fatalf("sse fallback call: %v %+v", err, res)
		}
	})

	t.Run("legacy-sse-explicit", func(t *testing.T) {
		port := freePort(t)
		startZooHTTP(t, "legacy-sse", port)
		cfg := &config.Config{Servers: map[string]config.ServerEntry{
			"h": {URL: fmt.Sprintf("http://127.0.0.1:%d/sse", port), Transport: "sse"},
		}}
		d := daemon.New(cfg, testLog())
		defer d.Close()
		tools, err := d.ListTools(ctx, "h")
		if err != nil || len(tools) != 2 {
			t.Fatalf("sse explicit list: tools=%d err=%v", len(tools), err)
		}
	})
}

func TestMain(m *testing.M) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		if _, err2 := os.Stat("/usr/local/bin/python3"); err2 != nil {
			// rely on PATH
		}
	}
	_ = strings.TrimSpace
	os.Exit(m.Run())
}
