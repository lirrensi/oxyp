package daemon

import (
	"context"
	"github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/metacache"
)

// ownerMux serves the minimal owner REST surface for federation tests.
func ownerMux(t *testing.T, instance string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"` + instance + `","servers":{"db":{"connected":true,"healthy":true},"github":{"connected":true,"healthy":true}}}`))
	})
	mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		// db = local; c:gamma = pinned transitive route (as a show-mode
		// owner would advertise via PeerNames). All consent to re-export.
		_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"Database","origin":"local","connected":true,"transitive":true},{"name":"github","description":"GH","origin":"local","connected":true,"transitive":true},{"name":"c:gamma","description":"Gamma","origin":"c","connected":true,"transitive":true}]}`))
	})
	mux.HandleFunc("GET /servers/{name}/tools", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tools":[{"name":"query","description":"Run SQL"}]}`))
	})
	mux.HandleFunc("GET /servers/{name}/tools/{tool}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"query","description":"Run SQL","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}}`))
	})
	mux.HandleFunc("POST /servers/{name}/tools/{tool}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"owner-result"}],"isError":false}`))
	})
	mux.HandleFunc("GET /servers/{name}/resources", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resources":[],"instructions":"own-instr"}`))
	})
	mux.HandleFunc("GET /servers/{name}/prompts", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"prompts":[]}`))
	})
	return mux
}

func mergeDaemon(t *testing.T, ownerURL string, peerCfg config.PeerEntry, local map[string]config.ServerEntry) *Daemon {
	t.Helper()
	peerCfg.URL = ownerURL
	cfg := &config.Config{
		Servers: local,
		Peers:   map[string]config.PeerEntry{"wsl": peerCfg},
	}
	d := New(cfg, testLog())
	t.Cleanup(d.Close)
	d.SyncPeers(context.Background())
	return d
}

func TestPeerMergeExpansion(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{
		"github": {Command: []string{"true"}},
	})
	names := d.Names()
	// Merge: peer db claims bare (no local rival); local github wins its bare;
	// qualified forms of winners hide from listings.
	want := map[string]bool{"db": true, "gamma": true, "github": true, "wsl:github": true}
	if len(names) != len(want) {
		t.Fatalf("names = %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected name %q in %v", n, names)
		}
	}
	if sh := d.Shadows("db"); len(sh) != 0 {
		t.Fatalf("uncontested bare must have no shadows: %v", sh)
	}
	// Qualified always routes direct.
	e := d.Entry("wsl:db")
	if e == nil || e.Peer != "wsl" || e.PeerOwner != "db" {
		t.Fatalf("qualified entry = %+v", e)
	}
	// Discovery + invoke proxy to the owner.
	tools, err := d.ListTools(context.Background(), "wsl:db")
	if err != nil || len(tools) != 1 || tools[0].Name != "query" {
		t.Fatalf("list = %+v %v", tools, err)
	}
	if tools[0].InputSchema.Properties == nil {
		t.Fatalf("describe schema must ride: %+v", tools[0].InputSchema)
	}
	res, err := d.CallTool(context.Background(), "wsl:db", "query", map[string]any{"q": "1"})
	if err != nil || len(res.Content) != 1 {
		t.Fatalf("call = %+v %v", res, err)
	}
	// Bare alias serves the winner.
	if _, err := d.ListTools(context.Background(), "db"); err != nil {
		t.Fatalf("bare list: %v", err)
	}
	// Search displays bare, routes either form.
	hits, _, _, err := d.SearchTools("", "query", false, 12, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Server] = true
	}
	if !seen["db"] || seen["wsl:db"] {
		t.Fatalf("search display = %v", seen)
	}
	if _, _, _, err := d.SearchTools("wsl:db", "query", false, 12, 0); err != nil {
		t.Fatalf("qualified search: %v", err)
	}
}

func TestPeerMergePriority(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	neg := -10
	d := mergeDaemon(t, owner.URL, config.PeerEntry{
		Servers: map[string]config.PeerServer{"github": {Priority: &neg}},
	}, map[string]config.ServerEntry{
		"github": {Command: []string{"true"}},
	})
	// Peer wins: bare github resolves to wsl:github, local hides.
	e := d.Entry("github")
	if e == nil || e.Peer != "wsl" {
		t.Fatalf("bare github must resolve to peer, got %+v", e)
	}
	if sh := d.Shadows("github"); len(sh) != 1 || sh[0] != "github" {
		t.Fatalf("shadows = %v", sh)
	}
	for _, n := range d.Names() {
		if n == "wsl:github" {
			t.Fatalf("winner qualified must hide from listings: %v", d.Names())
		}
	}
}

func TestPeerNamespaceMode(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{Mode: config.PeerModeNamespace},
		map[string]config.ServerEntry{})
	for _, n := range d.Names() {
		if !strings.Contains(n, ":") {
			t.Fatalf("namespace mode must list qualified only: %v", d.Names())
		}
	}
	if d.Entry("db") != nil {
		t.Fatal("namespace mode must claim no bare name")
	}
}

func TestPeerIncludeFilter(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{Mode: config.PeerModeNamespace, Include: []string{"db"}},
		map[string]config.ServerEntry{})
	if got := d.Names(); len(got) != 1 || got[0] != "wsl:db" {
		t.Fatalf("include must narrow: %v", got)
	}
}

func TestPeerDownKeepsStale(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if len(d.Names()) == 0 {
		t.Fatal("want virtuals after sync")
	}
	owner.Close() // peer dies
	d.SyncPeers(context.Background())
	if d.Entry("wsl:db") == nil {
		t.Fatal("dead peer must keep last-good virtuals")
	}
	h := d.HealthOf("wsl:db")
	if h == nil || h.Healthy || !strings.Contains(h.Error, "unreachable") {
		t.Fatalf("down health = %+v", h)
	}
	if sts := d.PeerStatuses(); len(sts) != 1 || sts[0].Down == "" {
		t.Fatalf("peer status = %+v", sts)
	}
}

func TestPeerSelfMountRefused(t *testing.T) {
	d := New(&config.Config{
		Peers: map[string]config.PeerEntry{"self": {URL: "http://127.0.0.1:9"}},
	}, testLog())
	defer d.Close()
	// Point the client's health at our own instance id.
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"` + d.InstanceID() + `","servers":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"servers":[]}`))
	}))
	defer owner.Close()
	d.mu.Lock()
	if pr, ok := d.peersMap["self"]; ok {
		pr.cfg.URL = owner.URL
	}
	d.mu.Unlock()
	d.SyncPeers(context.Background())
	if sts := d.PeerStatuses(); len(sts) != 1 || sts[0].Down == "" {
		t.Fatalf("self-mount must mark down: %+v", sts)
	}
}

func TestPeerReloadSurvives(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	cfg := &config.Config{
		Servers: map[string]config.ServerEntry{"a": {Command: []string{"true"}}},
		Peers:   map[string]config.PeerEntry{"wsl": {URL: owner.URL}},
	}
	d := New(cfg, testLog())
	defer d.Close()
	d.SyncPeers(context.Background())
	if d.Entry("wsl:db") == nil {
		t.Fatal("want virtuals")
	}
	// Reload with an added local: virtuals must survive.
	cfg2 := &config.Config{
		Servers: map[string]config.ServerEntry{
			"a": {Command: []string{"true"}},
			"b": {Command: []string{"true"}},
		},
		Peers: map[string]config.PeerEntry{"wsl": {URL: owner.URL}},
	}
	added, removed, _ := d.Reload(cfg2)
	if len(added) != 1 || len(removed) != 0 {
		t.Fatalf("reload = +%v -%v", added, removed)
	}
	if d.Entry("wsl:db") == nil {
		t.Fatal("reload must not unmount virtuals")
	}
	// Removing the peer config drops its virtuals on next sync.
	cfg3 := &config.Config{
		Servers: map[string]config.ServerEntry{"a": {Command: []string{"true"}}},
	}
	d.Reload(cfg3)
	d.SyncPeers(context.Background())
	if d.Entry("wsl:db") != nil {
		t.Fatal("removed peer must unmount")
	}
}

func TestPeerFingerprintSeparation(t *testing.T) {
	a := config.ServerEntry{Name: "wsl:db", Peer: "wsl", PeerOwner: "db"}
	b := config.ServerEntry{Name: "corp:db", Peer: "corp", PeerOwner: "db"}
	if metacache.Fingerprint(a) == metacache.Fingerprint(b) {
		t.Fatal("peer identity must affect fingerprint")
	}
	c := config.ServerEntry{Command: []string{"true"}}
	d := c
	d.Peer, d.PeerOwner = "wsl", "db"
	if metacache.Fingerprint(c) == metacache.Fingerprint(d) {
		t.Fatal("peer marker must flip fingerprint")
	}
}

func TestSharedWithPeerMatrix(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	mk := func(ps *config.PeerSharingConfig) *Daemon {
		cfg := &config.Config{
			Daemon: config.DaemonConfig{PeerSharing: ps},
			Servers: map[string]config.ServerEntry{
				"pub":    {Command: []string{"true"}},
				"secret": {Command: []string{"true"}},
			},
			Peers: map[string]config.PeerEntry{"wsl": {URL: owner.URL}},
		}
		d := New(cfg, testLog())
		t.Cleanup(d.Close)
		d.SyncPeers(context.Background())
		return d
	}
	// Default (nil): locals shared, virtuals hidden (bare + qualified).
	d := mk(nil)
	for _, n := range []string{"pub", "secret"} {
		if !d.SharedWithPeer(n) {
			t.Fatalf("default must share local %q", n)
		}
	}
	for _, n := range []string{"db", "wsl:db", "wsl:github", "nope", "local:nope"} {
		if d.SharedWithPeer(n) {
			t.Fatalf("default must hide %q", n)
		}
	}
	// Show: virtuals shared too (bare merge display + qualified).
	d = mk(&config.PeerSharingConfig{Mode: config.PeerSharingShow})
	for _, n := range []string{"pub", "db", "wsl:db"} {
		if !d.SharedWithPeer(n) {
			t.Fatalf("show must share %q", n)
		}
	}
	// Deny wins over show and over allow.
	d = mk(&config.PeerSharingConfig{Mode: config.PeerSharingShow,
		Allow: []string{"*"}, Deny: []string{"*secret", "db"}})
	if d.SharedWithPeer("secret") || d.SharedWithPeer("db") {
		t.Fatal("deny must win")
	}
	if !d.SharedWithPeer("pub") {
		t.Fatal("non-denied must stay shared")
	}
	// Allow-list mode: only matches shared.
	d = mk(&config.PeerSharingConfig{Allow: []string{"pub*"}})
	if !d.SharedWithPeer("pub") || d.SharedWithPeer("secret") {
		t.Fatal("allow-list must narrow")
	}
	// local: hatch strips to its local.
	d = mk(nil)
	if !d.SharedWithPeer("local:pub") || d.SharedWithPeer("local:db") {
		t.Fatal("local: must strip to locals only")
	}
}

func TestTransitivePinnedRoute(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	// Pinned route imports as peer:c:gamma; leaf claims bare when uncontested.
	e := d.Entry("wsl:c:gamma")
	if e == nil || e.Peer != "wsl" || e.PeerOwner != "c:gamma" {
		t.Fatalf("pinned entry = %+v", e)
	}
	bare := d.Entry("gamma")
	if bare == nil || bare.PeerOwner != "c:gamma" {
		t.Fatalf("bare leaf must resolve pinned, got %+v", bare)
	}
	// Include matches route or leaf.
	d2 := mergeDaemon(t, owner.URL, config.PeerEntry{Include: []string{"gamma"}}, map[string]config.ServerEntry{})
	if d2.Entry("wsl:c:gamma") == nil {
		t.Fatal("include leaf must match pinned route")
	}
	d3 := mergeDaemon(t, owner.URL, config.PeerEntry{Include: []string{"c:gamma"}}, map[string]config.ServerEntry{})
	if d3.Entry("wsl:c:gamma") == nil {
		t.Fatal("include route must match pinned route")
	}
	d4 := mergeDaemon(t, owner.URL, config.PeerEntry{Include: []string{"db"}}, map[string]config.ServerEntry{})
	if d4.Entry("wsl:c:gamma") != nil {
		t.Fatal("narrow include must drop pinned route")
	}
}

func TestPeerNamesAdvertisePinned(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "owner1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{
		"mine": {Command: []string{"true"}},
	})
	got := d.PeerNames()
	want := map[string]bool{"mine": true, "wsl:c:gamma": true, "wsl:db": true, "wsl:github": true}
	if len(got) != len(want) {
		t.Fatalf("PeerNames = %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected advertised %q in %v", n, got)
		}
	}
}

func TestImportDepthCap(t *testing.T) {
	deep := strings.Repeat("p:", MaxImportColons+2) + "leaf"
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"o1","servers":{}}`))
		case "/servers":
			_, _ = w.Write([]byte(`{"servers":[{"name":"` + deep + `","description":"deep","connected":true}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if len(d.Names()) != 0 {
		t.Fatalf("over-deep route must be skipped: %v", d.Names())
	}
}

// instanceMux serves ownerMux catalog but with a fixed instance id,
// so two httptest URLs can pose as the same daemon.
func instanceMux(t *testing.T, instance string) *http.ServeMux {
	t.Helper()
	mux := ownerMux(t, instance)
	return mux
}

func TestCollapseDuplicatePeers(t *testing.T) {
	// Same instance behind two URLs = same daemon mounted twice.
	o1 := httptest.NewServer(instanceMux(t, "dup-instance-1"))
	defer o1.Close()
	o2 := httptest.NewServer(instanceMux(t, "dup-instance-1"))
	defer o2.Close()
	cfg := &config.Config{
		Peers: map[string]config.PeerEntry{
			"zed": {URL: o1.URL},
			"abc": {URL: o2.URL},
		},
	}
	d := New(cfg, testLog())
	defer d.Close()
	d.SyncPeers(context.Background())
	// Lexically-first wins; loser down with pointer, virtuals dropped.
	sts := map[string]PeerStatus{}
	for _, st := range d.PeerStatuses() {
		sts[st.Name] = st
	}
	if sts["abc"].Down != "" {
		t.Fatalf("winner must be up: %+v", sts["abc"])
	}
	if !strings.Contains(sts["zed"].Down, "duplicate of abc") {
		t.Fatalf("loser must point at winner: %+v", sts["zed"])
	}
	if d.Entry("zed:db") != nil {
		t.Fatal("loser virtuals must drop")
	}
	if d.Entry("abc:db") == nil {
		t.Fatal("winner virtuals must survive")
	}
	// Winner vanishes → loser resyncs live (no stuck down).
	o2.Close()
	d.SyncPeers(context.Background())
	// o2 dead: abc goes unreachable-down, zed still dup-down (abc probed?
	// no — abc failed, so no instance to collide; zed must revive).
	// Actually: abc down (unreachable) is skipped by collapse, so zed revives.
	sts = map[string]PeerStatus{}
	for _, st := range d.PeerStatuses() {
		sts[st.Name] = st
	}
	if !strings.Contains(sts["abc"].Down, "refused") && !strings.Contains(sts["abc"].Down, "connect") {
		t.Logf("abc down = %q (want unreachable)", sts["abc"].Down)
	}
	if sts["zed"].Down != "" {
		t.Fatalf("loser must revive when winner dies: %+v", sts["zed"])
	}
	if d.Entry("zed:db") == nil {
		t.Fatal("revived loser must remount virtuals")
	}
}

func TestCollapseSkipsDistinctInstances(t *testing.T) {
	o1 := httptest.NewServer(instanceMux(t, "inst-A"))
	defer o1.Close()
	o2 := httptest.NewServer(instanceMux(t, "inst-B"))
	defer o2.Close()
	cfg := &config.Config{
		Peers: map[string]config.PeerEntry{
			"one": {URL: o1.URL},
			"two": {URL: o2.URL},
		},
	}
	d := New(cfg, testLog())
	defer d.Close()
	d.SyncPeers(context.Background())
	for _, st := range d.PeerStatuses() {
		if st.Down != "" {
			t.Fatalf("distinct instances must both stay up: %+v", st)
		}
	}
	// Same leaf via different peers stays unique (no content dedup, ever).
	if d.Entry("one:db") == nil || d.Entry("two:db") == nil {
		t.Fatal("same leaf via distinct peers must both exist")
	}
}

func TestRebindOnURLChange(t *testing.T) {
	var aCalls, bCalls atomic.Int64
	mk := func(calls *atomic.Int64) *httptest.Server {
		mux := ownerMux(t, "rebind-1")
		inner := mux
		wrapped := http.NewServeMux()
		wrapped.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/servers/") && strings.Contains(r.URL.Path, "/tools/") {
				calls.Add(1)
			}
			inner.ServeHTTP(w, r)
		}))
		return httptest.NewServer(wrapped)
	}
	srvA := mk(&aCalls)
	defer srvA.Close()
	srvB := mk(&bCalls)
	defer srvB.Close()
	cfg := &config.Config{Peers: map[string]config.PeerEntry{"w": {URL: srvA.URL}}}
	d := New(cfg, testLog())
	defer d.Close()
	d.SyncPeers(context.Background())
	ctx := context.Background()
	if _, err := d.CallTool(ctx, "w:db", "query", nil); err != nil {
		t.Fatalf("call A: %v", err)
	}
	if aCalls.Load() != 1 || bCalls.Load() != 0 {
		t.Fatalf("want A=1 B=0, got %d %d", aCalls.Load(), bCalls.Load())
	}
	// Move the peer URL: live proxies must follow, no restart.
	cfg2 := &config.Config{Peers: map[string]config.PeerEntry{"w": {URL: srvB.URL}}}
	d.Reload(cfg2)
	d.SyncPeers(ctx)
	if _, err := d.CallTool(ctx, "w:db", "query", nil); err != nil {
		t.Fatalf("call B: %v", err)
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatalf("want A=1 B=1 after rebind, got %d %d", aCalls.Load(), bCalls.Load())
	}
}

func TestSyncReloadRace(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "race-1"))
	defer owner.Close()
	cfg := &config.Config{Peers: map[string]config.PeerEntry{"w": {URL: owner.URL}}}
	d := New(cfg, testLog())
	defer d.Close()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			d.SyncPeers(ctx)
			cfg2 := &config.Config{Peers: map[string]config.PeerEntry{"w": {URL: owner.URL, Priority: i}}}
			d.Reload(cfg2)
		}(i)
	}
	wg.Wait()
}

func TestOwnerHealthMergeAndClear(t *testing.T) {
	var mode atomic.Int64 // 0=auth, 1=healthy
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"hm-1","servers":{"db":{"connected":true}}}`))
		case "/servers":
			if mode.Load() == 0 {
				_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"d","auth_required":"run: oxyp login db"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"d"}]}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	cfg := &config.Config{Peers: map[string]config.PeerEntry{"w": {URL: owner.URL}}}
	d := New(cfg, testLog())
	defer d.Close()
	ctx := context.Background()
	d.SyncPeers(ctx)
	h := d.HealthOf("w:db")
	if h == nil || h.AuthRequired == "" {
		t.Fatalf("sync must surface owner auth: %+v", h)
	}
	// Owner recovers with identical catalog shape: error must clear.
	mode.Store(1)
	d.SyncPeers(ctx)
	h = d.HealthOf("w:db")
	if h != nil && (h.AuthRequired != "" || h.Error != "") {
		t.Fatalf("recovery must clear owner-state: %+v", h)
	}
}

func TestPeerTimeoutMaterialized(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "to-1"))
	defer owner.Close()
	pt, st := 60, 5
	d := mergeDaemon(t, owner.URL, config.PeerEntry{Timeout: &pt,
		Servers: map[string]config.PeerServer{"db": {Timeout: &st}}},
		map[string]config.ServerEntry{})
	e := d.Entry("wsl:db")
	if e == nil || e.ToolTimeout == nil || *e.ToolTimeout != 5 {
		t.Fatalf("per-server timeout must win: %+v", e)
	}
	e = d.Entry("wsl:github")
	if e == nil || e.ToolTimeout == nil || *e.ToolTimeout != 60 {
		t.Fatalf("peer timeout must apply: %+v", e)
	}
	d2 := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if e := d2.Entry("wsl:db"); e == nil || e.ToolTimeout != nil {
		t.Fatalf("unset timeout must stay nil: %+v", e)
	}
}

func TestPeerSlowToolRespectsTimeout(t *testing.T) {
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"slow-1","servers":{}}`))
		case r.URL.Path == "/servers":
			_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"d"}]}`))
		case r.URL.Path == "/servers/db/tools":
			_, _ = w.Write([]byte(`{"tools":[{"name":"slow","description":"s"}]}`))
		case strings.HasPrefix(r.URL.Path, "/servers/db/tools/"):
			time.Sleep(1500 * time.Millisecond)
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"isError":false}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	// Default 10s: 1.5s tool passes (would die at 300ms with client Timeout).
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.CallTool(ctx, "wsl:db", "slow", nil); err != nil {
		t.Fatalf("1.5s tool must pass default timeout: %v", err)
	}
	// 1s virtual timeout: must die by ctx, not by client cap.
	one := 1
	d2 := mergeDaemon(t, owner.URL, config.PeerEntry{Timeout: &one}, map[string]config.ServerEntry{})
	if _, err := d2.CallTool(ctx, "wsl:db", "slow", nil); err == nil {
		t.Fatal("1s timeout must kill 1.5s tool")
	}
}

func TestBulkCatalogSync(t *testing.T) {
	var catalogCalls atomic.Int64
	var describeCalls atomic.Int64
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/federation/v1/catalog":
			catalogCalls.Add(1)
			_, _ = w.Write([]byte(`{"version":"v1","instance":"bulk-1","name":"o","servers":[{"name":"db","description":"d","tools":[{"name":"q","description":"run","inputSchema":{"type":"object","properties":{"x":{"type":"string"}}}}]}]}`))
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"bulk-1","servers":{}}`))
		case strings.HasPrefix(r.URL.Path, "/servers/db/tools/"):
			describeCalls.Add(1)
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if catalogCalls.Load() != 1 {
		t.Fatalf("want 1 catalog call, got %d", catalogCalls.Load())
	}
	// Cold describe served from seeded bulk meta: zero describes upstream.
	tools, err := d.ListTools(context.Background(), "wsl:db")
	if err != nil || len(tools) != 1 {
		t.Fatalf("list = %+v %v", tools, err)
	}
	if tools[0].InputSchema.Properties == nil {
		t.Fatalf("bulk schema must seed: %+v", tools[0].InputSchema)
	}
	if describeCalls.Load() != 0 {
		t.Fatalf("bulk sync must kill N+1, got %d describes", describeCalls.Load())
	}
}

func TestThinFallbackOldPeer(t *testing.T) {
	// No /federation/v1/catalog route (404) → thin /servers path, as before.
	owner := httptest.NewServer(ownerMux(t, "thin-1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if d.Entry("wsl:db") == nil {
		t.Fatal("thin fallback must mount")
	}
}

func TestPeerLocalPolicy(t *testing.T) {
	owner := httptest.NewServer(ownerMux(t, "pol-1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{
		Servers: map[string]config.PeerServer{
			"db": {
				Tools:  &config.ToolsPolicy{Deny: []string{"query"}},
				Params: map[string][]string{"other": {"a"}},
			},
		},
	}, map[string]config.ServerEntry{})
	e := d.Entry("wsl:db")
	if e == nil || e.Tools == nil || len(e.Tools.Deny) != 1 || len(e.Params) != 1 {
		t.Fatalf("policy must materialize: %+v", e)
	}
	// Local deny hides from search (gates read the virtual entry).
	hits, _, _, err := d.SearchTools("wsl:db", "query", false, 12, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("denied tool must be invisible: %+v", hits)
	}
}

func TestOriginConsentVeto(t *testing.T) {
	// db consents, priv does not. Show mode advertises db only.
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"cons-1","servers":{}}`))
		case "/servers":
			_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"d","transitive":true},{"name":"priv","description":"p"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	// Middle imports both, shows downstream: db re-exports, priv does not.
	midCfg := &config.Config{
		Daemon:  config.DaemonConfig{PeerSharing: &config.PeerSharingConfig{Mode: config.PeerSharingShow}},
		Peers:   map[string]config.PeerEntry{"o": {URL: owner.URL}},
		Servers: map[string]config.ServerEntry{},
	}
	mid := New(midCfg, testLog())
	defer mid.Close()
	mid.SyncPeers(context.Background())
	if !mid.SharedWithPeer("o:db") {
		t.Fatal("consented virtual must share in show mode")
	}
	if mid.SharedWithPeer("o:priv") {
		t.Fatal("unconsented virtual must not share even in show mode")
	}
	// Hide mode vetoes regardless of consent.
	midCfg2 := &config.Config{
		Daemon:  config.DaemonConfig{PeerSharing: &config.PeerSharingConfig{Mode: config.PeerSharingHide}},
		Peers:   map[string]config.PeerEntry{"o": {URL: owner.URL}},
		Servers: map[string]config.ServerEntry{},
	}
	mid2 := New(midCfg2, testLog())
	defer mid2.Close()
	mid2.SyncPeers(context.Background())
	if mid2.SharedWithPeer("o:db") {
		t.Fatal("hide must veto even consented virtuals")
	}
}

func TestV1ProxyFidelity(t *testing.T) {
	var v1calls atomic.Int64
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/federation/v1/catalog":
			_, _ = w.Write([]byte(`{"version":"v1","instance":"v1-1","name":"o","servers":[{"name":"db","description":"d","tools":[{"name":"shot","description":"takes","inputSchema":{"type":"object"},"annotations":{"title":"Shot","readOnlyHint":true}}]}]}`))
		case "/federation/v1/call":
			v1calls.Add(1)
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"v1-ok","annotations":{"audience":["user"]}},{"type":"image","data":"aGk=","mimeType":"image/png"}],"isError":false,"structuredContent":{"n":1},"_meta":{"trace":"x"}}`))
		case "/federation/v1/prompt":
			_, _ = w.Write([]byte(`{"description":"d","messages":[{"role":"user","content":{"type":"text","text":"hi"}}]}`))
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"v1-1","servers":{}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	// Annotations ride the catalog into the seeded tool.
	tools, err := d.ListTools(context.Background(), "wsl:db")
	if err != nil || len(tools) != 1 {
		t.Fatalf("list = %+v %v", tools, err)
	}
	ro := tools[0].Annotations.ReadOnlyHint
	if tools[0].Annotations.Title != "Shot" || ro == nil || !*ro {
		t.Fatalf("annotations lost: %+v", tools[0].Annotations)
	}
	// Call goes v1: media + structured + meta verbatim.
	res, err := d.CallTool(context.Background(), "wsl:db", "shot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v1calls.Load() != 1 || len(res.Content) != 2 || res.StructuredContent == nil || res.Meta == nil {
		t.Fatalf("v1 call = %+v calls=%d", res, v1calls.Load())
	}
	if _, ok := res.Content[1].(mcp.ImageContent); !ok {
		t.Fatalf("image block lost: %#v", res.Content[1])
	}
	// Prompts fetchable via v1 (no longer the advertised lie).
	pr, err := d.GetPrompt(context.Background(), "wsl:db", "g", nil)
	if err != nil || len(pr.Messages) != 1 {
		t.Fatalf("prompt = %+v %v", pr, err)
	}
}

func TestV1DowngradeToThin(t *testing.T) {
	// Catalog 404s (old peer) but thin works: proxy routes thin, prompts empty.
	owner := httptest.NewServer(ownerMux(t, "old-1"))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if _, err := d.ListTools(context.Background(), "wsl:db"); err != nil {
		t.Fatalf("thin list: %v", err)
	}
	if _, err := d.CallTool(context.Background(), "wsl:db", "query", nil); err != nil {
		t.Fatalf("thin call: %v", err)
	}
	pr, err := d.ListPrompts(context.Background(), "wsl:db")
	if err != nil || len(pr) != 0 {
		t.Fatalf("thin prompts must be empty: %+v %v", pr, err)
	}
	if _, err := d.GetPrompt(context.Background(), "wsl:db", "g", nil); err == nil {
		t.Fatal("thin GetPrompt must refuse")
	}
}

func TestSyncCoalescesMidFlight(t *testing.T) {
	// Slow first peer gates the running sync; a second peer added mid-flight
	// must mount after the in-flight pass (deferred, never dropped).
	release := make(chan struct{})
	var once sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"slow-1","servers":{}}`))
		case "/servers":
			_, _ = w.Write([]byte(`{"servers":[{"name":"s","description":"s"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer slow.Close()
	fast := httptest.NewServer(ownerMux(t, "fast-1"))
	defer fast.Close()
	d := New(&config.Config{Peers: map[string]config.PeerEntry{"a": {URL: slow.URL}}}, testLog())
	defer d.Close()
	done := make(chan struct{})
	go func() { d.SyncPeers(context.Background()); close(done) }()
	time.Sleep(200 * time.Millisecond) // let the slow sync start
	d.Reload(&config.Config{Peers: map[string]config.PeerEntry{
		"a": {URL: slow.URL}, "b": {URL: fast.URL},
	}})
	d.SyncPeers(context.Background()) // mid-flight: must defer, not drop
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("sync never finished")
	}
	// Coalesced rerun mounts b (poll: the rerun is synchronous within SyncPeers).
	deadline := time.Now().Add(20 * time.Second)
	for d.Entry("b:db") == nil && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if d.Entry("b:db") == nil {
		t.Fatal("peer added mid-sync must mount after coalesced rerun")
	}
}

func TestDownPeerRetriesOnTick(t *testing.T) {
	// Peer down at boot: marked down, then a healthy tick mounts it
	// (pingPeer retry path for never-synced peers). Same URL throughout
	// (a Reload would clear down state — the point is tick recovery).
	var up atomic.Bool
	inner := ownerMux(t, "late-1")
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer owner.Close()
	d := New(&config.Config{Peers: map[string]config.PeerEntry{"w": {URL: owner.URL}}}, testLog())
	defer d.Close()
	d.SyncPeers(context.Background())
	if st := d.PeerStatuses()[0]; st.Down == "" {
		t.Fatalf("want down, got %+v", st)
	}
	up.Store(true) // owner revives at the same URL
	d.mu.Lock()
	if pr, ok := d.peersMap["w"]; ok {
		pr.retryAt = time.Now().Add(-time.Second)
	}
	d.mu.Unlock()
	d.pingPeer(context.Background(), "w", nil)
	deadline := time.Now().Add(20 * time.Second)
	for d.Entry("w:db") == nil && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if d.Entry("w:db") == nil {
		t.Fatal("revived peer must mount on tick retry")
	}
}

func TestAliasSamePeerLeafOrder(t *testing.T) {
	// One peer advertising two routes with the same leaf: the bare winner
	// must be total-ordered (qualified compare), never map-order luck.
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"alias-1","servers":{}}`))
		case "/servers":
			_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"d"},{"name":"c:db","description":"d2"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	var first string
	for range 10 {
		d := New(&config.Config{Peers: map[string]config.PeerEntry{"w": {URL: owner.URL}}}, testLog())
		d.SyncPeers(context.Background())
		e := d.Entry("db")
		d.Close()
		if e == nil {
			t.Fatal("bare db must resolve")
		}
		if first == "" {
			first = e.PeerOwner
		} else if e.PeerOwner != first {
			t.Fatalf("winner flipped: %q vs %q", first, e.PeerOwner)
		}
	}
	if first != "c:db" {
		t.Fatalf("lexical qualified must win (w:c:db < w:db)? got %q", first)
	}
}

func TestEmptyCatalogClearsCaches(t *testing.T) {
	// Owner empties a server: next sync clears tools (authoritative empty),
	// not keeps last-good.
	var empty atomic.Bool
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/federation/v1/catalog":
			tools := `[{"name":"q","description":"d","inputSchema":{"type":"object"}}]`
			if empty.Load() {
				tools = `[]`
			}
			_, _ = w.Write([]byte(`{"version":"v1","instance":"clr-1","name":"o","rev":"r","servers":[{"name":"db","description":"d","tools":` + tools + `}]}`))
		case "/health":
			_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"clr-1","servers":{}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer owner.Close()
	d := mergeDaemon(t, owner.URL, config.PeerEntry{}, map[string]config.ServerEntry{})
	if tools, _ := d.ListTools(context.Background(), "wsl:db"); len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	empty.Store(true)
	d.SyncPeers(context.Background())
	if tools, _ := d.ListTools(context.Background(), "wsl:db"); len(tools) != 0 {
		t.Fatalf("emptied owner must clear tools, got %d", len(tools))
	}
}
