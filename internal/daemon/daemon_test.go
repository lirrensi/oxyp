package daemon

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewSkipsDisabledAndFlagsBad(t *testing.T) {
	f := false
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"on":  {Command: []string{"true"}},
		"off": {Command: []string{"true"}, Enabled: &f},
		"bad": {ConfigError: "nope"},
	}}
	d := New(cfg, testLog())
	if len(d.Names()) != 2 {
		t.Fatalf("want on+bad, got %v", d.Names())
	}
	if h := d.HealthOf("bad"); h == nil || h.Healthy || h.Error == "" {
		t.Fatalf("bad server should be unhealthy: %+v", h)
	}
	if _, err := d.ListTools(context.Background(), "bad"); err == nil {
		t.Fatal("misconfigured server must refuse list")
	}
}

func TestReloadKeepsUnchanged(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"keep": {Command: []string{"true"}},
		"drop": {Command: []string{"true"}},
	}}, testLog())
	added, removed, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"keep": {Command: []string{"true"}},
		"new":  {Command: []string{"true"}},
	}})
	if len(added) != 1 || added[0] != "new" {
		t.Fatalf("added = %v", added)
	}
	if len(removed) != 1 || removed[0] != "drop" {
		t.Fatalf("removed = %v", removed)
	}
	if len(modified) != 0 {
		t.Fatalf("modified = %v", modified)
	}
	if d.Entry("drop") != nil || d.Entry("new") == nil || d.Entry("keep") == nil {
		t.Fatal("reload map wrong")
	}
}

func TestReloadDetectsModify(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	_, _, modified := d.Reload(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"false"}},
	}})
	if len(modified) != 1 {
		t.Fatalf("modified = %v", modified)
	}
}

func TestColdByDefault(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	// PingLoop must not spawn anything: run one tick manually via pingConnected.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.pingConnected(ctx)
	m := d.servers["s"]
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		t.Fatal("ping must not connect cold servers")
	}
}
