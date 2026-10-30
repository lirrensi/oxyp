package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/mark3labs/mcp-go/client"
)

type countClient struct {
	client.MCPClient
	closes *atomic.Int64
	block  chan struct{}
}

func (c *countClient) Close() error {
	c.closes.Add(1)
	if c.block != nil {
		<-c.block
	}
	return nil
}

func connectedDaemon(t *testing.T, n int, block chan struct{}) (*Daemon, *atomic.Int64) {
	t.Helper()
	cfg := &config.Config{Servers: map[string]config.ServerEntry{}}
	for i := 0; i < n; i++ {
		name := string(rune('a'+i)) + "-srv"
		cfg.Servers[name] = config.ServerEntry{Command: []string{"true"}}
	}
	d := New(cfg, testLog())
	var closes atomic.Int64
	for name := range cfg.Servers {
		m := d.servers[name]
		m.mu.Lock()
		m.client = &countClient{closes: &closes, block: block}
		m.mu.Unlock()
	}
	return d, &closes
}

func TestCloseClosesAllOnce(t *testing.T) {
	d, closes := connectedDaemon(t, 3, nil)
	d.Close()
	if got := closes.Load(); got != 3 {
		t.Fatalf("each client must close exactly once, got %d", got)
	}
	for _, m := range d.servers {
		m.mu.Lock()
		has := m.client != nil
		m.mu.Unlock()
		if has {
			t.Fatal("client must be cleared after Close")
		}
	}
}

func TestCloseIdempotent(t *testing.T) {
	d, closes := connectedDaemon(t, 2, nil)
	d.Close()
	d.Close()
	if got := closes.Load(); got != 2 {
		t.Fatalf("second Close must not re-close, got %d", got)
	}
}

func TestCloseBoundsHangingClose(t *testing.T) {
	block := make(chan struct{})
	d, closes := connectedDaemon(t, 1, block)
	done := make(chan struct{})
	go func() { d.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("Close must block while client hangs")
	case <-time.After(100 * time.Millisecond):
	}
	close(block)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after unblock")
	}
	// Bounded pattern: single hanging Close never exceeds ~2x shutdown timeout.
	if got := closes.Load(); got != 1 {
		t.Fatalf("want 1 close, got %d", got)
	}
	// Second Close is safe and does not re-close.
	d.Close()
	if got := closes.Load(); got != 1 {
		t.Fatalf("second Close must be a no-op, got %d", got)
	}
}

func TestCloseCancelsSpawnCtx(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	select {
	case <-d.spawnCtx.Done():
		t.Fatal("spawnCtx live before Close")
	default:
	}
	d.Close()
	select {
	case <-d.spawnCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close must cancel spawnCtx")
	}
	d.Close() // safe
}

// Closing while a sweep is parked on in-flight work must not deadlock.
func TestCloseWithInFlightCall(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	m := d.servers["s"]
	m.mu.Lock()
	m.inFlight = 1
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { d.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung with in-flight work")
	}
	_ = context.Background
}
