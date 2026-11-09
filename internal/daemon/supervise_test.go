package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
)

// superviseTestDaemon builds a daemon with one dead-remote server (nothing
// listens on 127.0.0.1:9 — discard port, refused fast).
func superviseTestDaemon(t *testing.T) (*Daemon, *managed) {
	t.Helper()
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"dead": {Name: "dead", URL: "http://127.0.0.1:9/mcp"},
	}}, testLog())
	m := d.managed("dead")
	if m == nil {
		t.Fatal("dead server missing")
	}
	return d, m
}

func TestSuperviseFailureDegradedThenDown(t *testing.T) {
	d, m := superviseTestDaemon(t)
	defer d.Close()
	err := errors.New("connection refused")
	for i := 1; i <= degradedRetries; i++ {
		d.superviseFailure(m, err)
		h := d.HealthOf("dead")
		if h == nil || h.State != stateDegraded || h.Retries != i {
			t.Fatalf("fail %d: want degraded/%d, got %+v", i, i, h)
		}
		if h.Error == "" || h.RetryIn == "" {
			t.Fatalf("fail %d: error/retry_in must be set: %+v", i, h)
		}
	}
	d.superviseFailure(m, err)
	if h := d.HealthOf("dead"); h == nil || h.State != stateDown {
		t.Fatalf("fail %d: want down, got %+v", degradedRetries+1, h)
	}
}

func TestSuperviseClearsBackoff(t *testing.T) {
	d, m := superviseTestDaemon(t)
	defer d.Close()
	d.superviseFailure(m, errors.New("boom"))
	m.supervise()
	h := d.HealthOf("dead")
	if h == nil || !h.Healthy || h.State != "" {
		t.Fatalf("success must stamp clean healthy: %+v", h)
	}
	m.mu.Lock()
	n, at := m.failCount, m.retryAt
	m.mu.Unlock()
	if n != 0 || !at.IsZero() {
		t.Fatalf("success must clear backoff: count=%d retryAt=%v", n, at)
	}
}

func TestSuperviseAuthParksWithoutBackoff(t *testing.T) {
	d, m := superviseTestDaemon(t)
	defer d.Close()
	d.superviseFailure(m, &AuthNeededError{Server: "dead"})
	h := d.HealthOf("dead")
	if h == nil || h.State != stateAuthNeeded || h.AuthRequired == "" {
		t.Fatalf("auth must park auth-needed: %+v", h)
	}
	if h.Retries != 0 || h.RetryIn != "" {
		t.Fatalf("auth must not arm backoff: %+v", h)
	}
	// healDue must skip parked auth: no retryAt, no spawn attempt.
	m.mu.Lock()
	zero := m.retryAt.IsZero()
	m.mu.Unlock()
	if !zero {
		t.Fatal("auth park must leave retryAt zero")
	}
}

func TestHealDueSkipsColdHealthyParked(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"cold": {Name: "cold", Command: []string{"true"}},
		"ok":   {Name: "ok", Command: []string{"true"}},
		"auth": {Name: "auth", URL: "http://127.0.0.1:9/mcp"},
		"cfgx": {Name: "cfgx", ConfigError: "bad entry"},
	}}, testLog())
	defer d.Close()
	d.managed("ok").supervise()
	d.superviseFailure(d.managed("auth"), &AuthNeededError{Server: "auth"})
	d.healDue(context.Background())
	if h := d.HealthOf("cold"); h != nil {
		t.Fatalf("cold must stay untouched: %+v", h)
	}
	if h := d.HealthOf("ok"); h == nil || !h.Healthy {
		t.Fatalf("healthy must stay healthy: %+v", h)
	}
	if h := d.HealthOf("auth"); h == nil || h.State != stateAuthNeeded {
		t.Fatalf("auth park must survive heal tick: %+v", h)
	}
	if h := d.HealthOf("cfgx"); h == nil || h.State != stateConfigError {
		t.Fatalf("config park must survive heal tick: %+v", h)
	}
}

func TestHealDueRetriesDegraded(t *testing.T) {
	d, m := superviseTestDaemon(t)
	defer d.Close()
	// Arm degraded with an elapsed backoff.
	d.superviseFailure(m, errors.New("refused"))
	m.mu.Lock()
	m.retryAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	d.healDue(context.Background())
	h := d.HealthOf("dead")
	if h == nil || h.State != stateDegraded || h.Retries != 2 {
		t.Fatalf("heal attempt on dead server must re-arm degraded/2: %+v", h)
	}
}

func TestHealDueSkipsFutureBackoff(t *testing.T) {
	d, m := superviseTestDaemon(t)
	defer d.Close()
	d.superviseFailure(m, errors.New("refused"))
	// retryAt is ~1s in the future: healDue must not probe yet.
	d.healDue(context.Background())
	if h := d.HealthOf("dead"); h == nil || h.Retries != 1 {
		t.Fatalf("future backoff must not probe: %+v", h)
	}
}

func TestHealOneRespawnsDeadChild(t *testing.T) {
	// A stdio server whose binary is missing: heal tries to respawn and
	// re-arms (proves the respawn path runs, failure re-arms backoff).
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {Name: "s", Command: []string{"oxyp-test-no-such-binary-xyz"}},
	}}, testLog())
	defer d.Close()
	m := d.managed("s")
	d.superviseFailure(m, errors.New("spawn failed"))
	m.mu.Lock()
	m.retryAt = time.Now().Add(-time.Second)
	m.mu.Unlock()
	d.healOne(context.Background(), m)
	if h := d.HealthOf("s"); h == nil || h.State != stateDegraded || h.Retries != 2 {
		t.Fatalf("failed respawn must re-arm degraded/2: %+v", h)
	}
}
