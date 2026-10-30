package metacache

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/lirrensi/oxyp/internal/config"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := New(filepath.Join(dir, "cache.json"))
	tools := []mcp.Tool{{Name: "a", Description: "x"}, {Name: "b"}}
	c.Put("s", "fp1", tools)

	c2 := New(filepath.Join(dir, "cache.json"))
	got, ok := c2.Get("s", "fp1")
	if !ok || len(got) != 2 || got[0].Name != "a" {
		t.Fatalf("round trip failed: ok=%v got=%+v", ok, got)
	}
	if _, ok := c2.Get("s", "other-fp"); ok {
		t.Fatal("fingerprint mismatch must miss")
	}
	if _, ok := c2.Get("nope", "fp1"); ok {
		t.Fatal("unknown server must miss")
	}
}

func TestCorruptStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(p, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(p)
	if _, ok := c.Get("s", "fp"); ok {
		t.Fatal("corrupt cache must start empty")
	}
	// and stay usable
	c.Put("s", "fp", []mcp.Tool{{Name: "a"}})
	if _, ok := c.Get("s", "fp"); !ok {
		t.Fatal("put after corrupt failed")
	}
}

func TestDisabledCache(t *testing.T) {
	c := New("")
	c.Put("s", "fp", []mcp.Tool{{Name: "a"}})
	if _, ok := c.Get("s", "fp"); !ok {
		t.Fatal("disabled cache should still work in-memory")
	}
}

func TestFingerprintIgnoresPolicy(t *testing.T) {
	a := config.ServerEntry{Command: []string{"x"}}
	b := config.ServerEntry{Command: []string{"x"}, Lifecycle: "keep-alive",
		Description: "other", Tools: &config.ToolsPolicy{Deny: []string{"*"}}}
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("lifecycle/desc/tools must not affect fingerprint")
	}
	c := config.ServerEntry{Command: []string{"y"}}
	if Fingerprint(a) == Fingerprint(c) {
		t.Fatal("command change must affect fingerprint")
	}
	d := config.ServerEntry{URL: "https://x/mcp", Headers: map[string]string{"A": "1"}}
	e := config.ServerEntry{URL: "https://x/mcp", Headers: map[string]string{"A": "2"}}
	if Fingerprint(d) == Fingerprint(e) {
		t.Fatal("header change must affect fingerprint")
	}
}

func TestDropStale(t *testing.T) {
	dir := t.TempDir()
	c := New(filepath.Join(dir, "cache.json"))
	c.Put("keep", "fp1", nil)
	c.Put("stale", "fp-old", nil)
	c.Put("gone", "fp", nil)
	c.DropStale(map[string]string{"keep": "fp1", "stale": "fp-new"})
	if _, ok := c.Get("keep", "fp1"); !ok {
		t.Fatal("keep dropped")
	}
	if _, ok := c.Get("stale", "fp-new"); ok {
		t.Fatal("stale not dropped")
	}
	if _, ok := c.Get("gone", "fp"); ok {
		t.Fatal("gone not dropped")
	}
}

func TestConcurrentSavesSafe(t *testing.T) {
	dir := t.TempDir()
	c := New(filepath.Join(dir, "cache.json"))
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "s" + string(rune('a'+i))
			c.Put(name, "fp", []mcp.Tool{{Name: name}})
		}(i)
	}
	wg.Wait()
	c2 := New(filepath.Join(dir, "cache.json"))
	n := 0
	for i := range 20 {
		name := "s" + string(rune('a'+i))
		if _, ok := c2.Get(name, "fp"); ok {
			n++
		}
	}
	if n == 0 {
		t.Fatal("concurrent saves lost everything")
	}
}
