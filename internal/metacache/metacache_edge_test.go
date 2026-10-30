package metacache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"oxyp/internal/config"
)

func TestDropPersists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	c := New(p)
	c.Put("a", "fp-a", []mcp.Tool{{Name: "a"}})
	c.Put("b", "fp-b", []mcp.Tool{{Name: "b"}})
	c.Drop("a")
	if _, ok := c.Get("a", "fp-a"); ok {
		t.Fatal("dropped entry must miss in memory")
	}
	if _, ok := c.Get("b", "fp-b"); !ok {
		t.Fatal("sibling entry must survive Drop")
	}
	// persisted: reopen, a stays gone, b stays.
	c2 := New(p)
	if _, ok := c2.Get("a", "fp-a"); ok {
		t.Fatal("Drop must persist: a reappeared after reopen")
	}
	if _, ok := c2.Get("b", "fp-b"); !ok {
		t.Fatal("b lost after reopen")
	}
	// dropping a missing name is a no-op that still persists cleanly.
	c2.Drop("nope")
	if _, ok := New(p).Get("b", "fp-b"); !ok {
		t.Fatal("Drop of missing name corrupted the file")
	}
}

func TestDropStaleEmptyAndUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	c := New(p)
	c.Put("a", "fp", []mcp.Tool{{Name: "a"}})
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Empty map: everything stale → file rewritten without servers.
	c.DropStale(map[string]string{})
	if _, ok := c.Get("a", "fp"); ok {
		t.Fatal("empty current map must drop everything")
	}
	// Unchanged fingerprints: no rewrite (mtime stable).
	c.Put("a", "fp", []mcp.Tool{{Name: "a"}})
	time.Sleep(20 * time.Millisecond)
	mtime1, _ := os.Stat(p)
	c.DropStale(map[string]string{"a": "fp"})
	mtime2, _ := os.Stat(p)
	if !mtime2.ModTime().Equal(mtime1.ModTime()) {
		t.Fatal("DropStale with no changes must not rewrite the file")
	}
	_ = before
}

func TestFileModeAndNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "cache.json")
	c := New(p)
	c.Put("a", "fp", []mcp.Tool{{Name: "a"}})
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o, want 600", st.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if n != "cache.json" {
			t.Fatalf("leftover temp file after Put: %s", n)
		}
	}
	// 20 more puts: still exactly one file, mode stable.
	for range 20 {
		c.Put("a", "fp", []mcp.Tool{{Name: "a"}})
	}
	entries, _ = os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp leftovers after repeated Put: %v", entries)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode drifted: %o", st.Mode().Perm())
	}
}

func TestCachedAtPersisted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	c := New(p)
	before := time.Now().Add(-time.Second)
	c.Put("a", "fp", []mcp.Tool{{Name: "a"}})
	after := time.Now().Add(time.Second)
	var f File
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	e, ok := f.Servers["a"]
	if !ok {
		t.Fatal("entry missing on disk")
	}
	if e.CachedAt.Before(before) || e.CachedAt.After(after) {
		t.Fatalf("CachedAt %v outside [%v,%v]", e.CachedAt, before, after)
	}
	if e.Fingerprint != "fp" {
		t.Fatalf("Fingerprint on disk = %q", e.Fingerprint)
	}
}

func TestNilToolsSemantics(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	c := New(p)
	// nil tools round-trip as miss=false with nil slice (documented).
	c.Put("n", "fp", nil)
	got, ok := c.Get("n", "fp")
	if !ok {
		t.Fatal("nil-tools entry must hit on matching fingerprint")
	}
	if got != nil {
		t.Fatalf("nil tools must come back nil, got %#v", got)
	}
	c2 := New(p)
	got, ok = c2.Get("n", "fp")
	if !ok || got != nil {
		t.Fatalf("nil tools must survive disk round trip: ok=%v got=%#v", ok, got)
	}
	// empty vs nil: Put(nil) then Get returns nil; Put([]) returns empty non-nil.
	c.Put("e", "fp", []mcp.Tool{})
	got, ok = c.Get("e", "fp")
	if !ok {
		t.Fatal("empty-tools entry must hit")
	}
	t.Logf("current behavior: empty tools round trip as %#v", got)
	// disabled cache keeps nil semantics in memory.
	d := New("")
	d.Put("n", "fp", nil)
	if got, ok := d.Get("n", "fp"); !ok || got != nil {
		t.Fatalf("disabled cache nil-tools: ok=%v got=%#v", ok, got)
	}
}

func TestFingerprintFieldMatrix(t *testing.T) {
	base := config.ServerEntry{Command: []string{"bin"}, URL: "https://h/mcp"}
	fp := Fingerprint(base)
	same := func(mut func(*config.ServerEntry)) bool {
		e := base
		mut(&e)
		return Fingerprint(e) == fp
	}
	// identity-relevant: each flips the fingerprint.
	flips := map[string]func(*config.ServerEntry){
		"command":   func(e *config.ServerEntry) { e.Command = []string{"other"} },
		"url":       func(e *config.ServerEntry) { e.URL = "https://other/mcp" },
		"transport": func(e *config.ServerEntry) { e.Transport = "sse" },
		"headers":   func(e *config.ServerEntry) { e.Headers = map[string]string{"A": "1"} },
		"env":       func(e *config.ServerEntry) { e.Env = map[string]string{"K": "V"} },
		"cwd":       func(e *config.ServerEntry) { e.Cwd = "/tmp" },
		"auth":      func(e *config.ServerEntry) { e.Auth = "oauth" },
		"tls":       func(e *config.ServerEntry) { e.TLS = &config.TLSConfig{CAFile: "x"} },
		"oauth-id":  func(e *config.ServerEntry) { e.OAuth = &config.OAuthConfig{ClientID: "a"} },
	}
	for name, mut := range flips {
		if same(mut) {
			t.Errorf("field %s must affect fingerprint", name)
		}
	}
	// ignored: fingerprint stable.
	ignored := map[string]func(*config.ServerEntry){
		"lifecycle":   func(e *config.ServerEntry) { e.Lifecycle = "keep-alive" },
		"description": func(e *config.ServerEntry) { e.Description = "d" },
		"tools":       func(e *config.ServerEntry) { e.Tools = &config.ToolsPolicy{Deny: []string{"*"}} },
	}
	for name, mut := range ignored {
		if !same(mut) {
			t.Errorf("field %s must NOT affect fingerprint", name)
		}
	}
	// oauth client_secret excluded; other oauth fields included.
	sec := base
	sec.OAuth = &config.OAuthConfig{ClientID: "id", ClientSecret: "s1"}
	sec2 := base
	sec2.OAuth = &config.OAuthConfig{ClientID: "id", ClientSecret: "s2"}
	if Fingerprint(sec) != Fingerprint(sec2) {
		t.Error("OAuth client_secret must be excluded from fingerprint")
	}
	scope := base
	scope.OAuth = &config.OAuthConfig{ClientID: "id", Scope: "a b"}
	if Fingerprint(scope) == Fingerprint(base) {
		t.Error("OAuth scope must affect fingerprint")
	}
	// order stability: map iteration must not leak into the hash.
	h1 := config.ServerEntry{Command: []string{"b"}, Headers: map[string]string{"Z": "1", "A": "2"}, Env: map[string]string{"Y": "1", "B": "2"}}
	for range 25 {
		if Fingerprint(h1) != Fingerprint(h1) {
			t.Fatal("fingerprint unstable across calls (map order leak)")
		}
	}
}

func TestMalformedEntryStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	// valid JSON but wrong shape / null servers → start empty, stay usable.
	for _, body := range []string{`null`, `[]`, `{"servers":null}`, `{"servers":[]}`, `{"other":1}`} {
		p := filepath.Join(t.TempDir(), "cache.json")
		_ = dir
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		c := New(p)
		if _, ok := c.Get("s", "fp"); ok {
			t.Fatalf("body %s must start empty", body)
		}
		c.Put("s", "fp", []mcp.Tool{{Name: "a"}})
		if _, ok := c.Get("s", "fp"); !ok {
			t.Fatalf("cache unusable after body %s", body)
		}
	}
}

func TestConcurrentReaderWriter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	c := New(p)
	c.Put("shared", "fp", []mcp.Tool{{Name: "v0"}})
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 25 {
				_ = j
				c.Put("shared", "fp", []mcp.Tool{{Name: string(rune('a' + i))}})
				_, _ = c.Get("shared", "fp")
				c.DropStale(map[string]string{"shared": "fp"})
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 25 {
			_ = New(p)
		}
	}()
	wg.Wait()
	// final state must be coherent JSON with the shared entry (or dropped, never corrupt).
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("cache file corrupt after concurrent use: %v", err)
	}
}

// NOTE (gap, intentionally untested): Get returns the live Tools slice and
// Put stores the caller's slice without copying, so caller mutation races
// with save. A test pinning copy semantics would fail against the current
// code; the race is reported as a BUGFOUND gap instead of a passing test.
