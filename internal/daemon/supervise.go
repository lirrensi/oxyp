// Supervisor states for per-server resilience (lazy-first).
//
// Every local server (peer virtuals keep their owner/liveness merge) moves
// through cold → healthy → degraded → down, with two parked states that
// never auto-retry (a retry cannot fix them):
//
//	cold          never connected this generation (health == nil). Costs
//	              nothing: no pings, no polls, no retries. Only a named
//	              operation (tools/call/describe) leaves this state.
//	healthy       last operation or ping succeeded.
//	degraded      a transient failure happened (timeout, refused, reset,
//	              5xx, session-expired); a backoff retry is scheduled.
//	              Serving continues from cache where possible.
//	down          degraded exhausted its fast retries; a slow poll (1m)
//	              keeps probing until the server heals silently.
//	auth-needed   401/OAuth mid-flight or at spawn. Parked: needs
//	              `oxyp login <name>`, never retried.
//	config-broken ConfigError at load/reload. Parked: needs fix + reload.
//
// Only degraded/down servers consume background work, and only servers the
// operator actually woke can reach degraded/down (cold never auto-wakes).
package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"

	"github.com/lirrensi/oxyp/internal/redact"
)

// ServerState is the supervisor's per-server lifecycle state. Serialized
// as "state" on /health (cold/degraded/down/auth-needed/config-broken;
// healthy servers omit it for wire compat with older peers).
type ServerState string

const (
	stateHealthy     ServerState = "healthy"
	stateDegraded    ServerState = "degraded"
	stateDown        ServerState = "down"
	stateAuthNeeded  ServerState = "auth-needed"
	stateConfigError ServerState = "config-broken"
)

const (
	// degradedRetries is the fast-retry budget before a server is
	// declared down (backoff 1s,2s,4s,8s,16s ≈ 31s of trying).
	degradedRetries = 5
	// downPollInterval is the slow-heal cadence for down servers.
	downPollInterval = time.Minute
	// downPollTimeout bounds one slow-heal probe.
	downPollTimeout = 10 * time.Second
)

// supervise records a successful operation: clears degraded/down backoff,
// stamps healthy. Cheap and unconditional — every success heals.
func (m *managed) supervise() {
	m.mu.Lock()
	m.failCount = 0
	m.retryAt = time.Time{}
	m.mu.Unlock()
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now()})
}

// superviseLatency is supervise with a ping latency stamp.
func (m *managed) superviseLatency(lat time.Duration) {
	m.mu.Lock()
	m.failCount = 0
	m.retryAt = time.Time{}
	m.mu.Unlock()
	m.setHealth(&Health{Healthy: true, LastChecked: time.Now(), Latency: lat})
}

// superviseFailure records a failed operation and reports whether the
// caller should retry inline (session-expiry only) or how the supervisor
// parked the server. Auth + config errors park immediately without
// backoff; transient errors enter degraded with exponential backoff.
func (d *Daemon) superviseFailure(m *managed, err error) {
	var authErr *AuthNeededError
	if errors.As(err, &authErr) {
		m.setHealth(&Health{LastChecked: time.Now(),
			AuthRequired: "run: oxyp login " + authErr.Server,
			State:      stateAuthNeeded})
		return
	}
	m.mu.Lock()
	m.failCount++
	n := m.failCount
	m.mu.Unlock()
	backoff := time.Duration(1<<(n-1)) * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	state := stateDegraded
	if n > degradedRetries {
		state = stateDown
		backoff = downPollInterval
	}
	m.mu.Lock()
	m.retryAt = time.Now().Add(backoff)
	m.mu.Unlock()
	m.setHealth(&Health{LastChecked: time.Now(),
		Error:   redact.Body(err.Error()),
		State:   state,
		Retries: n,
		RetryIn: backoff.Round(time.Second).String(),
	})
}

// HealLoop is the supervisor's background tick: it retries degraded/down
// servers whose backoff elapsed. Cold servers (health == nil), healthy
// servers, and parked auth/config states are skipped — the loop only
// spends work where a retry can heal.
func (d *Daemon) HealLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.healDue(ctx)
		}
	}
}

func (d *Daemon) healDue(ctx context.Context) {
	now := time.Now()
	d.mu.RLock()
	var due []*managed
	for _, m := range d.servers {
		if m.getCfg().Peer != "" {
			continue // peer virtuals heal via catalog sync, not probes
		}
		m.healthMu.RLock()
		h := m.health
		m.healthMu.RUnlock()
		if h == nil || (h.State != stateDegraded && h.State != stateDown) {
			continue
		}
		m.mu.Lock()
		retryAt := m.retryAt
		inFlight := m.connecting != nil
		m.mu.Unlock()
		if retryAt.IsZero() || now.Before(retryAt) || inFlight {
			continue
		}
		due = append(due, m)
	}
	d.mu.RUnlock()
	for _, m := range due {
		d.healOne(ctx, m)
	}
}

// healOne probes a single degraded/down server: ping the live client when
// present, else re-spawn (the spawn path re-inits sessions, redials,
// restarts children). Success heals silently; failure re-arms backoff.
// Session-expiry resets the client first so the retry gets a fresh session.
func (d *Daemon) healOne(ctx context.Context, m *managed) {
	cfg := m.getCfg()
	m.mu.Lock()
	c := m.client
	m.mu.Unlock()
	// Heal probes are bounded tight (5s): a dead server must re-arm
	// fast, not eat the 30s init timeout serially per server.
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c == nil {
		if _, err := d.clientFor(hctx, m); err != nil {
			d.superviseFailure(m, err)
			return
		}
		m.supervise()
		d.log.Info("server healed (respawn)", "server", cfg.Name)
		return
	}
	pctx, pcancel := context.WithTimeout(ctx, downPollTimeout)
	defer pcancel()
	if err := c.Ping(pctx); err != nil {
		if cfg.IsRemote() && errors.Is(err, transport.ErrSessionTerminated) {
			d.log.Info("heal: session expired, re-initializing", "server", cfg.Name)
			d.resetClient(m)
			if _, serr := d.clientFor(hctx, m); serr != nil {
				d.superviseFailure(m, serr)
				return
			}
			m.supervise()
			d.log.Info("server healed (session re-init)", "server", cfg.Name)
			return
		}
		d.superviseFailure(m, err)
		return
	}
	m.supervise()
	d.log.Info("server healed (ping)", "server", cfg.Name)
}
