package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/metacache"
	peerfederation "github.com/lirrensi/oxyp/internal/peerfederation"
	"github.com/lirrensi/oxyp/internal/perms"
)

var errSelfMount = errors.New("peer is self (instance id match)")

type managedClient = client.MCPClient

// closeManagedLocked detaches a managed client's connection; caller holds d.mu.
func closeManagedLocked(m *managed) managedClient {
	m.mu.Lock()
	c := m.client
	m.client = nil
	m.spawnErr = nil
	m.mu.Unlock()
	return c
}

func closeManagedClients(cs []managedClient) {
	var out []client.MCPClient
	for _, c := range cs {
		if c != nil {
			out = append(out, c)
		}
	}
	closeClientsBounded(out)
}

// shortChain renders a visited chain as short ids for diagnostics.
func shortChain(chain []string) string {
	out := make([]string, 0, len(chain))
	for _, id := range chain {
		out = append(out, peerfederation.ShortID(id))
	}
	return strings.Join(out, " -> ")
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// spawnPeer validates the loop guard and returns a REST proxy client.
// The proxy initializes lazily on first RPC (health probe doubles as init).
func (d *Daemon) spawnPeer(ctx context.Context, m *managed, cfg config.ServerEntry) (client.MCPClient, error) {
	d.mu.RLock()
	self := d.instanceID
	pr, ok := d.peersMap[cfg.Peer]
	var pcl *peerfederation.Client
	var pinst string
	if ok && pr != nil {
		pcl, pinst = pr.client, pr.instance
	}
	d.mu.RUnlock()
	if !ok || pcl == nil {
		return nil, fmt.Errorf("server %q: peer %q not configured", cfg.Name, cfg.Peer)
	}
	chain := peerfederation.VisitedFrom(ctx)
	if peerfederation.CheckLoop(self, chain) {
		return nil, fmt.Errorf("server %q: federation loop refused (via %s)", cfg.Name, shortChain(chain))
	}
	if self != "" && pinst != "" && pinst == self {
		return nil, fmt.Errorf("server %q is self", cfg.Name)
	}
	if !peerfederation.CheckOutbound(self, pinst, chain) {
		return nil, fmt.Errorf("server %q: peer %q already visited (via %s)", cfg.Name, cfg.Peer, shortChain(chain))
	}
	p := peerfederation.NewProxy(pcl, cfg.Peer, cfg.PeerOwner, cfg.PeerV1)
	// Inline init: probe the owner so spawn failures surface here
	// (clientFor/reporting paths expect spawn-time errors, not lazy ones).
	initCtx, cancel := context.WithTimeout(ctx, d.initTO)
	defer cancel()
	initCtx = peerfederation.WithInstance(initCtx, self)
	if _, err := p.Initialize(initCtx, mcp.InitializeRequest{}); err != nil {
		return nil, err
	}
	// Liveness OK — but preserve owner-reported auth/error from the last
	// sync (a working connection doesn't clear "owner needs login").
	// Copy-on-write: replace the record instead of mutating the published
	// pointer (HealthOf hands copies out; in-place writes race readers).
	m.healthMu.Lock()
	cur := m.health
	m.healthMu.Unlock()
	nh := &Health{LastChecked: time.Now()}
	if cur != nil {
		nh.Error = cur.Error
		nh.AuthRequired = cur.AuthRequired
		nh.Latency = cur.Latency
	}
	nh.Healthy = nh.Error == "" && nh.AuthRequired == ""
	m.setHealth(nh)
	return p, nil
}

// pingPeer probes one peer's /health once and fans the result out to its
// connected virtuals: per-server owner-state merges via mergeOwnerHealth,
// liveness stamps Healthy+Latency only when the owner reports no error.
// peerNamesMissing returns configured peers absent from the connected map
// (cold peers that still deserve a rev-compare tick).
func (d *Daemon) peerNamesMissing(have map[string][]string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	for name, pr := range d.peersMap {
		if pr == nil || pr.client == nil {
			continue
		}
		// Down peers re-enter on backoff (retry path in pingPeer);
		// otherwise a peer down at boot never mounts (no other timer).
		if pr.down != "" && time.Now().Before(pr.retryAt) {
			continue
		}
		if _, ok := have[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

func (d *Daemon) pingPeer(ctx context.Context, peer string, names []string) {
	// Snapshot under one RLock (siblings syncOnePeer/spawnPeer do the same;
	// the client pointer is swapped by reloads — unlocked reads race).
	d.mu.RLock()
	pr, ok := d.peersMap[peer]
	var pcl *peerfederation.Client
	var wantRev, down string
	var synced bool
	self := d.instanceID
	if ok && pr != nil {
		pcl, wantRev, down, synced = pr.client, pr.rev, pr.down, pr.synced
	}
	d.mu.RUnlock()
	if !ok || pcl == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pctx = peerfederation.WithInstance(pctx, self)
	if down != "" && !synced {
		// Never-mounted peer, backoff elapsed (peerNamesMissing gates):
		// one cheap /health decides whether a full sync is worth it.
		if _, herr := pcl.Health(pctx); herr != nil {
			d.pushRetry(peer)
			return
		}
		d.log.Info("peer reachable, mounting", "peer", peer)
		go d.SyncPeers(context.Background())
		return
	}
	if wantRev != "" {
		if rev, rerr := pcl.Rev(pctx); rerr == nil && rev != "" && rev != wantRev {
			d.log.Info("peer catalog changed, resyncing", "peer", peer)
			// Adopt first, sync async: without this every tick re-fires
			// while the resync is in flight (single-flight drops the
			// dupes but the log + goroutine spam is real).
			d.mu.Lock()
			if cur, ok := d.peersMap[peer]; ok {
				cur.rev = rev
			}
			d.mu.Unlock()
			go d.SyncPeers(context.Background())
		}
		// Rev failures are silent: health below is the real signal.
	}
	start := time.Now()
	h, err := pcl.Health(pctx)
	lat := time.Since(start)
	d.mu.RLock()
	ms := make([]*managed, 0, len(names))
	for _, n := range names {
		if m, ok := d.servers[n]; ok {
			m.mu.Lock()
			connected := m.client != nil
			m.mu.Unlock()
			if connected {
				ms = append(ms, m)
			}
		}
	}
	d.mu.RUnlock()
	if err != nil {
		for _, m := range ms {
			m.setHealth(&Health{LastChecked: time.Now(), Error: shortErr(err)})
		}
		return
	}
	for _, m := range ms {
		owner := m.getCfg().PeerOwner
		sh, ok := h.Servers[owner]
		if !ok {
			m.setHealth(&Health{Healthy: true, LastChecked: time.Now(), Latency: lat})
			continue
		}
		// Owner-state first (error/auth), then liveness under it.
		var oh *Health
		switch {
		case sh.AuthRequired != "":
			oh = &Health{LastChecked: time.Now(), AuthRequired: sh.AuthRequired + " (on " + peer + ")"}
		case sh.Error != "":
			oh = &Health{LastChecked: time.Now(), Error: "peer " + peer + ": " + sh.Error}
		case sh.Healthy != nil && !*sh.Healthy:
			oh = &Health{LastChecked: time.Now(), Error: "peer " + peer + " reports unhealthy"}
		}
		m.mergeOwnerHealth(oh)
		if oh == nil {
			m.setHealth(&Health{Healthy: true, LastChecked: time.Now(), Latency: lat})
		}
	}
}

// pushRetry backs a down peer off: 30s, doubling to 5m. Sync success clears
// retryAt (markPeerDown/sync paths reset it via down="").
func (d *Daemon) pushRetry(peer string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.peersMap[peer]
	if !ok {
		return
	}
	// Double the remaining wait (min 30s, max 5m). Crude but stateless:
	// retryAt is the only memory, so doubling remaining approximates it.
	remaining := time.Until(cur.retryAt)
	next := 30 * time.Second
	if remaining > 0 {
		next = 2 * remaining
		if next > 5*time.Minute {
			next = 5 * time.Minute
		}
	}
	cur.retryAt = time.Now().Add(next)
}

// withFederation attaches this daemon's instance id to ctx for outbound
// peer calls (loop-guard carrier; no-op when id is empty).
func (d *Daemon) withFederation(ctx context.Context) context.Context {
	d.mu.RLock()
	self := d.instanceID
	d.mu.RUnlock()
	return peerfederation.WithInstance(ctx, self)
}

// LocalRank is the merge-mode rank of local servers: peers with negative
// priority beat locals, positive lose to them, zero ties to first-configured.
const LocalRank = 0

// peerRuntime is one configured peer's live state.
type peerRuntime struct {
	cfg      config.PeerEntry
	client   *peerfederation.Client
	instance string    // owner's instance id (loop guard), "" until probed
	down     string    // last sync failure ("" = ok)
	v1       bool      // bulk catalog worked at least once (v1 routes exist)
	rev      string    // catalog rev at last sync ("" = unknown/old peer)
	synced   bool      // a catalog (bulk or thin) mounted at least once
	retryAt  time.Time // next tick a down peer may be reprobed (backoff)
}

// peerSyncTimeout bounds one peer catalog fetch.
const peerSyncTimeout = 15 * time.Second

// MaxImportColons caps ':' segments in an imported qualified name
// (peer:…:leaf). One under the wire loop cap: depth stays bounded even
// when two show-mode peers resync against each other forever.
const MaxImportColons = peerfederation.MaxVisited - 1

// InstanceID returns this daemon's federation id (loop guard).
func (d *Daemon) InstanceID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.instanceID
}

// SetInstanceID pins the federation id (persistent identity.json value).
// Must run before serving / before any SyncPeers.
func (d *Daemon) SetInstanceID(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	d.instanceID = id
	d.mu.Unlock()
}

// DaemonName returns the configured federation name ("": anonymous).
func (d *Daemon) DaemonName() string {
	return d.DaemonConfig().Name
}

// PeerStatus is one peer's operator-facing state (no secrets).
type PeerStatus struct {
	Name     string
	URL      string
	Mode     string
	Priority int
	Enabled  bool
	Error    string // config error ("" = ok)
	Down     string // last sync failure ("" = ok)
	Instance string // owner's instance id ("" = unprobed)
	Servers  int    // live virtual servers
}

// PeerStatuses returns every configured peer's status, sorted by name.
func (d *Daemon) PeerStatuses() []PeerStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []PeerStatus
	counts := map[string]int{}
	for _, m := range d.servers {
		if peer := m.cfg.Peer; peer != "" {
			counts[peer]++
		}
	}
	for name, pr := range d.peersMap {
		out = append(out, PeerStatus{
			Name: name, URL: pr.cfg.URL, Mode: pr.cfg.ModeOr(),
			Priority: pr.cfg.Priority, Enabled: true,
			Error: pr.cfg.ConfigError, Down: pr.down,
			Instance: pr.instance, Servers: counts[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rebuildPeers swaps the peer runtime map from config. Clients are rebuilt
// only when url/token/tls changed; surviving clients keep their connection
// state. Virtual servers are NOT touched here — SyncPeers expands them.
func (d *Daemon) rebuildPeers(peers map[string]config.PeerEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.peersMap == nil {
		d.peersMap = map[string]*peerRuntime{}
	}
	for name, p := range peers {
		if !p.IsEnabled() || p.ConfigError != "" {
			continue
		}
		if old, ok := d.peersMap[name]; ok && peerConnEqual(old.cfg, p) {
			old.cfg = p.DeepCopy()
			continue
		}
		c, err := peerfederation.NewClient(p.URL, p.Token, p.TLS, d.reqTO)
		if err != nil {
			d.log.Warn("peer client rejected", "peer", name, "err", err)
			continue
		}
		if old, ok := d.peersMap[name]; ok {
			// Connection changed (URL/token/TLS): swap the client AND
			// rebind every live Proxy, or invocations keep hitting the
			// old host with the old token (split brain). New generation
			// also resets the known instance (re-probed on next sync).
			old.cfg = p.DeepCopy()
			old.client = c
			old.instance = ""
			old.down = ""
			d.rebindPeerLocked(name, c)
			continue
		}
		d.peersMap[name] = &peerRuntime{cfg: p.DeepCopy(), client: c}
	}
	for name := range d.peersMap {
		p, ok := peers[name]
		if !ok || !p.IsEnabled() || p.ConfigError != "" {
			delete(d.peersMap, name)
		}
	}
}

// rebindPeerLocked swaps the REST client on every live Proxy of one peer.
// Caller holds d.mu. Proxies share m.mu with the daemon: take each m.mu
// briefly; no network, no blocking.
func (d *Daemon) rebindPeerLocked(peer string, c *peerfederation.Client) {
	n := 0
	for _, m := range d.servers {
		if m.cfg.Peer != peer {
			continue
		}
		m.mu.Lock()
		if px, ok := m.client.(*peerfederation.Proxy); ok && px != nil {
			px.Rebind(c)
			n++
		}
		m.mu.Unlock()
	}
	if n > 0 {
		d.log.Info("peer connection rebound", "peer", peer, "proxies", n)
	}
}

// peerConnEqual reports whether two peer configs share connection identity
// (url/token/tls). Other changes (mode/priority/include) don't need a new client.
func peerConnEqual(a, b config.PeerEntry) bool {
	if a.URL != b.URL || a.Token != b.Token {
		return false
	}
	if (a.TLS == nil) != (b.TLS == nil) {
		return false
	}
	if a.TLS != nil && *a.TLS != *b.TLS {
		return false
	}
	return true
}

// SyncPeers expands every configured peer into virtual servers. It runs at
// startup (async), on reload, and on demand (refresh path). Each peer fetch
// is bounded; a dead peer keeps its last-good virtuals with down health
// (stale catalog, honest error) instead of vanishing.
//
// Callers must NOT hold d.mu.
func (d *Daemon) SyncPeers(ctx context.Context) {
	// Coalescing single-flight: reload storms must not stack syncs (each
	// sync is N peers x 15s), but a request arriving mid-sync is DEFERRED
	// (syncAgain), never dropped — the runner re-runs once with a fresh
	// peer snapshot. Dropping loses peers added mid-flight.
	d.syncMu.Lock()
	if d.syncing {
		d.syncAgain = true
		d.syncMu.Unlock()
		return
	}
	d.syncing = true
	d.syncMu.Unlock()
	for {
		d.syncPeersOnce(ctx)
		d.syncMu.Lock()
		if !d.syncAgain {
			d.syncing = false
			d.syncMu.Unlock()
			return
		}
		d.syncAgain = false
		d.syncMu.Unlock()
	}
}

// syncPeersOnce runs one full pass: per-peer sync, duplicate collapse,
// orphan sweep, alias rebuild. SyncPeers wraps it with coalescing.
func (d *Daemon) syncPeersOnce(ctx context.Context) {
	d.mu.RLock()
	names := make([]string, 0, len(d.peersMap))
	for name := range d.peersMap {
		names = append(names, name)
	}
	self := d.instanceID
	d.mu.RUnlock()
	sort.Strings(names)
	for _, name := range names {
		d.syncOnePeer(ctx, name, self)
	}
	d.collapseDuplicatePeers()
	d.dropOrphanVirtuals()
	d.rebuildAliases()
}

// collapseDuplicatePeers drops virtuals of peers that report the same
// instance id as another live peer: same daemon mounted twice (two URLs).
// Direct peers only — we dedup what we shook hands with, never guesses
// about transitive leaves. Winner = lexically-first name (deterministic, no
// flapping); losers go down with an honest pointer and resync if the winner
// vanishes. Empty instance (unprobed/old peer) never collapses.
func (d *Daemon) collapseDuplicatePeers() {
	d.mu.RLock()
	byInstance := map[string][]string{}
	for name, pr := range d.peersMap {
		if pr.instance == "" || pr.down != "" {
			continue
		}
		byInstance[pr.instance] = append(byInstance[pr.instance], name)
	}
	d.mu.RUnlock()
	for _, names := range byInstance {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		winner := names[0]
		for _, loser := range names[1:] {
			d.mu.RLock()
			pr, ok := d.peersMap[loser]
			d.mu.RUnlock()
			if !ok {
				continue
			}
			d.mu.Lock()
			pr.down = "duplicate of " + winner + " (same instance " + peerfederation.ShortID(pr.instance) + ")"
			d.mu.Unlock()
			d.dropPeerVirtuals(loser)
			d.log.Warn("peer duplicate collapsed", "loser", loser, "winner", winner)
		}
	}
}

// dropPeerVirtuals removes every virtual owned by one peer.
func (d *Daemon) dropPeerVirtuals(peer string) {
	d.mu.Lock()
	var toClose []managedClient
	for q, m := range d.servers {
		if m.cfg.Peer != peer {
			continue
		}
		toClose = append(toClose, closeManagedLocked(m))
		delete(d.servers, q)
		if d.cache != nil {
			d.cache.Drop(q)
		}
	}
	d.mu.Unlock()
	closeManagedClients(toClose)
}

// dropOrphanVirtuals removes virtuals whose peer is no longer configured.
func (d *Daemon) dropOrphanVirtuals() {
	d.mu.Lock()
	var dead []string
	for q, m := range d.servers {
		peer := m.cfg.Peer
		if peer == "" {
			continue
		}
		if _, ok := d.peersMap[peer]; !ok {
			dead = append(dead, q)
		}
	}
	var toClose []managedClient
	for _, q := range dead {
		if m, ok := d.servers[q]; ok {
			toClose = append(toClose, closeManagedLocked(m))
			delete(d.servers, q)
			if d.cache != nil {
				d.cache.Drop(q)
			}
		}
	}
	d.mu.Unlock()
	closeManagedClients(toClose)
}

// syncOnePeer fetches one peer catalog and materializes virtual servers.
func (d *Daemon) syncOnePeer(ctx context.Context, name, self string) {
	d.mu.RLock()
	pr, ok := d.peersMap[name]
	var pcl *peerfederation.Client
	var pcfg config.PeerEntry
	var revStart string
	if ok && pr != nil {
		pcl, pcfg = pr.client, pr.cfg.DeepCopy()
		revStart = pr.rev
	}
	d.mu.RUnlock()
	if !ok || pcl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, peerSyncTimeout)
	defer cancel()
	ctx = peerfederation.WithInstance(ctx, self)

	// Bulk catalog first (one round trip: servers + tools + schemas +
	// instructions + health). Old peers 404 → thin fallback below.
	var instance string
	var cat *peerfederation.Catalog
	cat, cerr := pcl.Catalog(ctx)
	if cerr == nil && cat != nil {
		instance = cat.Instance
	} else {
		var pe *peerfederation.PeerError
		if !(errors.As(cerr, &pe) && pe.Status == http.StatusNotFound) {
			// Non-404 catalog failure = peer trouble, not old peer.
			// Still try thin before declaring down (catalog may 500
			// while /servers works — degraded but mountable).
			d.log.Info("peer catalog failed, trying thin", "peer", name, "err", shortErr(cerr))
		}
		h, herr := pcl.Health(ctx)
		svs, serr := pcl.Servers(ctx)
		if serr != nil {
			d.markPeerDown(name, serr)
			return
		}
		// Health is best-effort (instance id for the loop guard); the
		// catalog is what matters. A peer too old to report instance
		// still mounts.
		if herr == nil && h != nil {
			instance = h.Instance
		}
		d.expandThin(name, pcfg, svs, instance, self)
		d.mu.Lock()
		if cur, ok := d.peersMap[name]; ok {
			cur.rev = ""
		}
		d.mu.Unlock()
		return
	}
	// Stored rev is the catalog's own rev — but only when no tick adopted
	// a newer one meanwhile (slow sync must not clobber a fast-tick adopt:
	// the observed tick rev reflects post-sync state, ours mid-sync).
	if cat.Rev != "" {
		d.mu.Lock()
		if cur, ok := d.peersMap[name]; ok && cur.rev == revStart {
			cur.rev = cat.Rev
		}
		d.mu.Unlock()
	}
	d.expandBulk(name, pcfg, cat, instance, self, pr)
}

// expandBulk materializes a bulk catalog: virtuals + owner health +
// instructions + pre-seeded tool metadata (no N+1 describes later).
func (d *Daemon) expandBulk(name string, pcfg config.PeerEntry, cat *peerfederation.Catalog, instance, self string, pr *peerRuntime) {
	if instance != "" && instance == self {
		d.markPeerDown(name, errSelfMount)
		d.dropPeerVirtuals(name)
		return
	}
	d.mu.Lock()
	if cur, ok := d.peersMap[name]; ok && cur == pr {
		cur.instance = instance
		cur.down = ""
		cur.v1 = true
		cur.synced = true
		cur.retryAt = time.Time{}
	}
	d.mu.Unlock()

	want := map[string]config.ServerEntry{}
	health := map[string]*Health{}
	metas := map[string][]mcp.Tool{}
	instrs := map[string]string{}
	bulkRes := map[string][]mcp.Resource{}
	bulkTmpl := map[string][]mcp.ResourceTemplate{}
	bulkPrompts := map[string][]mcp.Prompt{}
	for _, s := range cat.Servers {
		if !peerIncluded(pcfg, s.Name) {
			continue
		}
		// Import depth cap (colons in the QUALIFIED name): without it,
		// bidir show-mode peers ping-pong routes one segment longer on
		// every resync (a:b → b:a:b → a:b:a:b …). MaxImportColons keeps
		// names bounded; the loop guard still stops any call cycle.
		q := config.Qualify(name, s.Name)
		if strings.Count(q, ":") > MaxImportColons {
			d.log.Warn("peer route too deep, skipping", "peer", name, "route", s.Name)
			continue
		}
		e := config.ServerEntry{
			Name: q, Description: s.Description,
			Peer: name, PeerOwner: s.Name,
			PeerTransitive: s.Transitive,
			PeerV1:         true,
		}
		// Importer-side overrides ride the virtual entry: timeout feeds
		// CallTool's ctx deadline; tools/params feed the standard gates
		// (deny-invisible + 403, params-403) before the owner re-checks.
		if t := pcfg.ServerTimeout(s.Name); t != nil {
			tc := *t
			e.ToolTimeout = &tc
		}
		if pol := pcfg.ServerPolicy(s.Name); pol != nil {
			tp := *pol
			tp.Allow = append([]string(nil), pol.Allow...)
			tp.Deny = append([]string(nil), pol.Deny...)
			e.Tools = &tp
		}
		if prm := pcfg.ServerParams(s.Name); prm != nil {
			pm := make(map[string][]string, len(prm))
			for k, v := range prm {
				pm[k] = append([]string(nil), v...)
			}
			e.Params = pm
		}
		want[q] = e
		// Owner health rides through, attributed; auth hints name the owner.
		if hrec := ownerHealth(name, s.AuthRequired, s.Error, s.Healthy); hrec != nil {
			health[q] = hrec
		}
		if len(s.Tools) != 0 {
			tools := make([]mcp.Tool, 0, len(s.Tools))
			for _, ct := range s.Tools {
				tools = append(tools, ct.ToTool())
			}
			metas[q] = tools
		} else if s.Error == "" && s.AuthRequired == "" {
			// Authoritative empty: a bulk row that listed successfully with
			// zero tools clears last-good. Unlistable rows (owner error)
			// skip seeding so last-good survives (stale-honest).
			metas[q] = []mcp.Tool{}
		}
		// Resources/templates/prompts/instructions: bulk rows are always
		// authoritative (best-effort fill, nil = none known) — absent or
		// empty clears last-good so removals propagate.
		instrs[q] = s.Instructions
		if s.Resources != nil {
			bulkRes[q] = s.Resources
		} else {
			bulkRes[q] = []mcp.Resource{}
		}
		if s.Templates != nil {
			bulkTmpl[q] = s.Templates
		} else {
			bulkTmpl[q] = []mcp.ResourceTemplate{}
		}
		if s.Prompts != nil {
			bulkPrompts[q] = s.Prompts
		} else {
			bulkPrompts[q] = []mcp.Prompt{}
		}
	}
	d.mergeVirtuals(name, want, health)
	d.seedBulkMeta(name, want, metas, instrs, bulkRes, bulkTmpl, bulkPrompts)
}

// expandThin materializes a thin /servers catalog (old peers without
// /federation/v1/catalog): virtuals + owner health, no tool metadata
// (cold lists describe on demand, as before).
func (d *Daemon) expandThin(name string, pcfg config.PeerEntry, svs []peerfederation.Server, instance, self string) {
	d.mu.RLock()
	pr, ok := d.peersMap[name]
	d.mu.RUnlock()
	if !ok {
		return
	}
	if instance != "" && instance == self {
		d.markPeerDown(name, errSelfMount)
		d.dropPeerVirtuals(name)
		return
	}
	d.mu.Lock()
	if cur, ok := d.peersMap[name]; ok && cur == pr {
		cur.instance = instance
		cur.down = ""
		cur.v1 = false
		cur.synced = true
		cur.retryAt = time.Time{}
	}
	d.mu.Unlock()

	want := map[string]config.ServerEntry{}
	health := map[string]*Health{}
	thinInstr := map[string]string{}
	for _, s := range svs {
		if !peerIncluded(pcfg, s.Name) {
			continue
		}
		q := config.Qualify(name, s.Name)
		if strings.Count(q, ":") > MaxImportColons {
			d.log.Warn("peer route too deep, skipping", "peer", name, "route", s.Name)
			continue
		}
		e := config.ServerEntry{
			Name: q, Description: s.Description,
			Peer: name, PeerOwner: s.Name,
			PeerTransitive: s.Transitive,
		}
		if t := pcfg.ServerTimeout(s.Name); t != nil {
			tc := *t
			e.ToolTimeout = &tc
		}
		if pol := pcfg.ServerPolicy(s.Name); pol != nil {
			tp := *pol
			tp.Allow = append([]string(nil), pol.Allow...)
			tp.Deny = append([]string(nil), pol.Deny...)
			e.Tools = &tp
		}
		if prm := pcfg.ServerParams(s.Name); prm != nil {
			pm := make(map[string][]string, len(prm))
			for k, v := range prm {
				pm[k] = append([]string(nil), v...)
			}
			e.Params = pm
		}
		want[q] = e
		if hrec := ownerHealth(name, s.AuthRequired, s.Error, s.Healthy); hrec != nil {
			health[q] = hrec
		}
		// Stash instructions for post-merge seeding (virtuals don't exist
		// until mergeVirtuals runs below — a pre-merge lookup always misses
		// on first mount).
		if s.Instructions != "" {
			thinInstr[q] = s.Instructions
		}
	}
	d.mergeVirtuals(name, want, health)
	d.seedThinInstr(name, want, thinInstr)
}

// seedThinInstr seeds thin-path instructions after mergeVirtuals creates
// the virtuals (mirrors the bulk path's post-merge seeding).
func (d *Daemon) seedThinInstr(peer string, want map[string]config.ServerEntry, thinInstr map[string]string) {
	if len(thinInstr) == 0 {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	// NOTE: thinInstr's own keys — `want` is drained by mergeVirtuals.
	for q, ins := range thinInstr {
		m, ok := d.servers[q]
		if !ok || m.getCfg().Peer != peer {
			continue
		}
		m.resMu.Lock()
		m.instructions = ins
		m.resMu.Unlock()
	}
}

// ownerHealth builds the attributed owner-state record for one catalog row.
// Nil = owner reports healthy (clears stuck owner-state on merge).
func ownerHealth(peer, authRequired, errMsg string, healthy *bool) *Health {
	switch {
	case authRequired != "":
		return &Health{LastChecked: time.Now(), AuthRequired: authRequired + " (on " + peer + ")"}
	case errMsg != "":
		return &Health{LastChecked: time.Now(), Error: "peer " + peer + ": " + errMsg}
	case healthy != nil && !*healthy:
		return &Health{LastChecked: time.Now(), Error: "peer " + peer + " reports unhealthy"}
	}
	return nil
}

// seedBulkMeta pre-seeds tool metadata + instructions for bulk-imported
// virtuals: cold lists/search/describe work without spawning or N+1.
func (d *Daemon) seedBulkMeta(peer string, want map[string]config.ServerEntry, metas map[string][]mcp.Tool, instrs map[string]string, bulkRes map[string][]mcp.Resource, bulkTmpl map[string][]mcp.ResourceTemplate, bulkPrompts map[string][]mcp.Prompt) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// NOTE: iterate the meta maps' own keys — `want` is drained by
	// mergeVirtuals (deletes + new-entry loop consume it), so ranging
	// `want` here would seed nothing.
	keys := map[string]bool{}
	for q := range metas {
		keys[q] = true
	}
	for q := range instrs {
		keys[q] = true
	}
	for q := range bulkRes {
		keys[q] = true
	}
	for q := range bulkTmpl {
		keys[q] = true
	}
	for q := range bulkPrompts {
		keys[q] = true
	}
	for q := range keys {
		m, ok := d.servers[q]
		if !ok || m.getCfg().Peer != peer {
			continue
		}
		if tools, ok := metas[q]; ok {
			m.toolsMu.Lock()
			m.tools = tools
			m.toolsCachedAt = time.Now()
			m.toolsMu.Unlock()
			if d.cache != nil {
				d.cache.Put(q, metacache.Fingerprint(m.getCfg()), tools)
			}
		}
		if ins, ok := instrs[q]; ok {
			m.resMu.Lock()
			m.instructions = ins
			m.resMu.Unlock()
		}
		// Bulk rows ride the full discovery surface: memory caches serve
		// cold without a spawn (disk cache stays tools-only, as locals).
		if res, ok := bulkRes[q]; ok {
			m.resMu.Lock()
			m.res = res
			m.resMu.Unlock()
		}
		if tmpl, ok := bulkTmpl[q]; ok {
			m.resMu.Lock()
			m.templates = tmpl
			m.resMu.Unlock()
		}
		if prm, ok := bulkPrompts[q]; ok {
			m.resMu.Lock()
			m.prompts = prm
			m.resMu.Unlock()
		}
	}
}

// peerIncluded reports whether a peer server passes the include filter.
// peerIncluded matches an advertised owner route against the include
// allowlist: full route (c:gamma) or leaf (gamma), exact, either hits.
func peerIncluded(p config.PeerEntry, server string) bool {
	if len(p.Include) == 0 {
		return true
	}
	leaf := server
	if i := strings.LastIndexByte(server, ':'); i >= 0 {
		leaf = server[i+1:]
	}
	for _, w := range p.Include {
		if w == server || w == leaf {
			return true
		}
	}
	return false
}

// mergeVirtuals swaps one peer's virtual servers into d.servers.
// Unchanged virtuals keep connections + caches; removed ones are closed.
func (d *Daemon) mergeVirtuals(peer string, want map[string]config.ServerEntry, health map[string]*Health) {
	d.mu.Lock()
	var toClose []managedClient
	for q, m := range d.servers {
		cfg := m.getCfg()
		if cfg.Peer != peer {
			continue
		}
		ne, ok := want[q]
		if !ok {
			toClose = append(toClose, closeManagedLocked(m))
			delete(d.servers, q)
			if d.cache != nil {
				d.cache.Drop(q)
			}
			continue
		}
		m.mu.Lock()
		changed := serverChanged(m.cfg, ne)
		if changed {
			m.cfg = ne
			m.gen++
		}
		m.mu.Unlock()
		if changed {
			toClose = append(toClose, closeManagedLocked(m))
			m.toolsMu.Lock()
			m.tools = nil
			m.toolsMu.Unlock()
			m.resMu.Lock()
			m.res = nil
			m.prompts = nil
			m.instructions = ""
			m.resMu.Unlock()
		}
		// Owner-state applies EVERY sync (present or cleared): the catalog
		// arriving healthy with identical tools must clear a stuck error.
		// Liveness (our connection) merges underneath — see mergeOwnerHealth.
		m.mergeOwnerHealth(health[q])
		delete(want, q)
	}
	for q, ne := range want {
		m := &managed{cfg: ne}
		// Fresh virtual: owner-state only (no liveness signal yet).
		// Healthy-absent (nil) stays nil = "never checked", like cold locals.
		if h, ok := health[q]; ok && h != nil {
			m.health = &Health{LastChecked: h.LastChecked, Error: h.Error, AuthRequired: h.AuthRequired}
		}
		d.servers[q] = m
	}
	// Refresh fingerprints so virtual caches survive DropStale.
	if d.cache != nil {
		fps := map[string]string{}
		for q, m := range d.servers {
			fps[q] = metacache.Fingerprint(m.getCfg())
		}
		d.cache.DropStale(fps)
	}
	d.mu.Unlock()
	closeManagedClients(toClose)
}

// mergeOwnerHealth folds one sync's owner-reported state into the virtual's
// health without clobbering our liveness signal:
//   - owner error/auth (h != nil): surface it; liveness stays as-is.
//     Healthy=false + owner Error/AuthRequired reads as "owner reports X".
//   - owner healthy (h == nil): clear owner Error/AuthRequired, keep
//     Healthy/liveness untouched (a healthy catalog doesn't prove our
//     connection works, and must not erase a live Healthy:true either).
//   - our own connection failures still overwrite via recordFailure/spawnPeer
//     (they carry no owner-state, so nothing to preserve).
func (m *managed) mergeOwnerHealth(h *Health) {
	m.healthMu.Lock()
	cur := m.health
	m.healthMu.Unlock()
	nh := &Health{LastChecked: time.Now()}
	if cur != nil {
		nh.Healthy = cur.Healthy
		nh.Latency = cur.Latency
	}
	if h == nil {
		if cur == nil {
			return
		}
		// Owner healthy: clear owner Error/AuthRequired, keep liveness.
		m.setHealth(nh)
		return
	}
	nh.Healthy = false
	nh.Error = h.Error
	nh.AuthRequired = h.AuthRequired
	m.setHealth(nh)
}

// markPeerDown records a sync failure: surviving virtuals keep serving
// stale metadata, each with an honest down error.
func (d *Daemon) markPeerDown(name string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if pr, ok := d.peersMap[name]; ok {
		pr.down = err.Error()
		if pr.retryAt.IsZero() || time.Now().After(pr.retryAt) {
			pr.retryAt = time.Now().Add(30 * time.Second)
		}
	}
	for _, m := range d.servers {
		if m.getCfg().Peer != name {
			continue
		}
		m.setHealth(&Health{LastChecked: time.Now(), Error: "peer " + name + " unreachable: " + shortErr(err)})
	}
}

// rebuildAliases recomputes the bare→canonical merge table from the current
// server map. Must run after any change to d.servers membership.
// Caller must NOT hold d.mu (it takes it).
func (d *Daemon) rebuildAliases() {
	d.mu.RLock()
	type cand struct {
		qualified string
		rank      int
		peer      string // "" = local
	}
	byBare := map[string][]cand{}
	for q, m := range d.servers {
		cfg := m.getCfg()
		if cfg.Peer == "" {
			// Locals never need an alias (bare IS canonical),
			// but they anchor priority: rank 0.
			if !strings.Contains(q, ":") {
				byBare[q] = append(byBare[q], cand{qualified: q, rank: LocalRank})
			}
			continue
		}
		// Namespace-mode peers claim no bare name.
		pr, ok := d.peersMap[cfg.Peer]
		if !ok || pr.cfg.ModeOr() != config.PeerModeMerge {
			continue
		}
		// Recursive naming: the bare claim is the LEAF (last segment).
		// A transitive owner route (c:gamma) arrives as peer:c:gamma and
		// competes on "gamma"; the pinned path stays qualified.
		bare := cfg.PeerOwner
		if i := strings.LastIndexByte(bare, ':'); i >= 0 {
			bare = bare[i+1:]
		}
		if bare == "" {
			continue
		}
		byBare[bare] = append(byBare[bare], cand{
			qualified: q, rank: pr.cfg.ServerPriority(cfg.PeerOwner), peer: cfg.Peer,
		})
	}
	d.mu.RUnlock()

	aliases := map[string]string{}
	shadows := map[string][]string{}
	for bare, cs := range byBare {
		if len(cs) == 1 && cs[0].peer == "" {
			continue // uncontested local: bare already canonical
		}
		if len(cs) == 1 {
			// Uncontested peer server: claim the bare name.
			aliases[bare] = cs[0].qualified
			continue
		}
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].rank != cs[j].rank {
				return cs[i].rank < cs[j].rank
			}
			// Deterministic tie-break: local beats peers, peers order
			// lexically, and same-peer routes order by qualified name
			// (one peer can advertise two routes with the same leaf —
			// without this the winner flips on map iteration order).
			if (cs[i].peer == "") != (cs[j].peer == "") {
				return cs[i].peer == ""
			}
			if cs[i].peer != cs[j].peer {
				return cs[i].peer < cs[j].peer
			}
			return cs[i].qualified < cs[j].qualified
		})
		// ToolHive rule: a bare name claimed by a peer that cannot win
		// (rank above all others) still shadows — but every claimant
		// stays reachable via qualified name, so no drop-all is needed.
		winner := cs[0].qualified
		if winner == bare {
			continue // local wins: bare already canonical
		}
		aliases[bare] = winner
		var losers []string
		for _, c := range cs[1:] {
			losers = append(losers, c.qualified)
		}
		sort.Strings(losers)
		shadows[bare] = losers
	}
	d.mu.Lock()
	d.aliases = aliases
	d.shadows = shadows
	d.mu.Unlock()
}

// SharedWithPeer reports whether a user-facing server name is advertised
// to peer daemons under [daemon.peer_sharing]. Non-peer callers bypass it
// (local policy is unchanged); peer callers get the export view:
//   - hide (default): peer-derived virtuals are never shared (depth-1);
//   - show: virtuals are shared too (transitive), subject to globs;
//   - deny globs always win; empty allow means allow-all; otherwise the
//     name must match an allow glob.
//
// Names match in bare display form (what /servers shows); the local:
// escape hatch strips to its local. Unknown names are not shared.
func (d *Daemon) SharedWithPeer(name string) bool {
	cfg := d.DaemonConfig()
	display, isPeer, consent := d.displayOrigin(name)
	return sharedWithPeer(cfg.PeerSharing, display, isPeer, consent)
}

// displayOrigin returns the bare display name, whether the entry is
// peer-derived, and whether a virtual carries origin re-export consent.
// Unknown → ("", false, false).
func (d *Daemon) displayOrigin(name string) (string, bool, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// local: escape hatch → its local entry (never peer-derived).
	if rest, ok := strings.CutPrefix(name, "local:"); ok {
		if m, hit := d.servers[rest]; hit && m.cfg.Peer == "" {
			return rest, false, false
		}
		return "", false, false
	}
	// Bare merge winner → canonical entry, display stays bare.
	if c, alias := d.aliases[name]; alias {
		if m, ok := d.servers[c]; ok {
			return name, m.cfg.Peer != "", m.cfg.PeerTransitive
		}
		return "", false, false
	}
	if m, ok := d.servers[name]; ok {
		return name, m.cfg.Peer != "", m.cfg.PeerTransitive
	}
	return "", false, false
}

func sharedWithPeer(ps *config.PeerSharingConfig, display string, isPeer, consent bool) bool {
	if display == "" {
		return false
	}
	mode := ps.ModeOr()
	if isPeer && mode != config.PeerSharingShow {
		return false
	}
	// Origin consent: a virtual re-exports downstream only when the owner
	// advertised transitive=true. Both sides veto (owner flag + our mode).
	if isPeer && !consent {
		return false
	}
	if ps == nil {
		return true
	}
	leaf := display
	if i := strings.LastIndexByte(display, ':'); i >= 0 {
		leaf = display[i+1:]
	}
	if _, hit := perms.MatchAny(ps.Deny, display); hit {
		return false
	}
	if leaf != display {
		if _, hit := perms.MatchAny(ps.Deny, leaf); hit {
			return false
		}
	}
	if len(ps.Allow) == 0 {
		return true
	}
	if _, hit := perms.MatchAny(ps.Allow, display); hit {
		return true
	}
	if leaf != display {
		if _, hit := perms.MatchAny(ps.Allow, leaf); hit {
			return true
		}
	}
	return false
}

// resolve maps a user-facing name to its canonical map key.
// Bare merge winners resolve to their qualified entry; local:<name> strips
// to the local; everything else passes through. Second return reports
// whether a rewrite applied.
func (d *Daemon) resolve(name string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if c, ok := d.aliases[name]; ok {
		return c, true
	}
	if rest, ok := strings.CutPrefix(name, "local:"); ok {
		if m, hit := d.servers[rest]; hit && m.cfg.Peer == "" {
			return rest, true
		}
	}
	return name, false
}

// ResolvedName reports the canonical key for a user-facing name.
func (d *Daemon) ResolvedName(name string) string {
	c, _ := d.resolve(name)
	return c
}

// Shadows reports the qualified losers shadowed by a bare merge name.
func (d *Daemon) Shadows(bare string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]string(nil), d.shadows[bare]...)
}
