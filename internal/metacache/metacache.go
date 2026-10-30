// Package metacache persists downstream tool metadata to disk so
// list/describe/search work without live connections and survive restarts.
//
// One JSON file (<oxypdir>/cache.json): server name → tools + definition
// fingerprint. An entry is valid only while the server's effective
// definition (command/url/env/headers/auth/bits that change identity)
// matches; config changes invalidate. Atomic writes (temp+rename), 0600.
package metacache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/lirrensi/oxyp/internal/config"
)

// Entry is one server's cached discovery.
type Entry struct {
	Fingerprint string     `json:"fingerprint"`
	CachedAt    time.Time  `json:"cachedAt"`
	Tools       []mcp.Tool `json:"tools"`
}

// File is the on-disk shape.
type File struct {
	Servers map[string]Entry `json:"servers"`
}

// Cache is a concurrency-safe disk-backed metadata store.
// Path "" = disabled (memory-only no-op for tests).
type Cache struct {
	mu     sync.RWMutex
	saveMu sync.Mutex
	path   string
	data   map[string]Entry
}

// New loads path if it exists (corrupt = start empty, never fatal).
func New(path string) *Cache {
	c := &Cache{data: map[string]Entry{}}
	if path == "" {
		return c
	}
	c.path = path
	b, err := os.ReadFile(path)
	if err != nil {
		return c // missing = fine
	}
	var f File
	if json.Unmarshal(b, &f) != nil || f.Servers == nil {
		return c // corrupt = start empty
	}
	c.data = f.Servers
	return c
}

// Fingerprint hashes the identity-relevant parts of an entry.
// Lifecycle/idle/description/tools-policy don't count (they don't change
// what the server offers).
func Fingerprint(s config.ServerEntry) string {
	var b strings.Builder
	b.WriteString(strings.Join(s.Command, "\x00"))
	b.WriteString("\x01" + s.URL + "\x01" + s.Transport)
	for _, k := range sortedKeys(s.Headers) {
		b.WriteString("\x02" + k + "=" + s.Headers[k])
	}
	for _, k := range sortedKeys(s.Env) {
		b.WriteString("\x03" + k + "=" + s.Env[k])
	}
	b.WriteString("\x03cwd=" + s.Cwd)
	if s.InheritEnv != nil {
		fmt.Fprintf(&b, "\x03inherit=%v", *s.InheritEnv)
	}
	b.WriteString("\x04" + s.Auth)
	if s.TLS != nil {
		b.WriteString(fmt.Sprintf("\x05%+v", *s.TLS))
	}
	if s.OAuth != nil {
		// secret excluded: same endpoint+client ⇒ same tools
		fmt.Fprintf(&b, "\x06%s|%s|%d|%s|%d", s.OAuth.ClientID,
			s.OAuth.Scope, s.OAuth.CallbackPort, s.OAuth.Flow, s.OAuth.Timeout)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Get returns a COPY of cached tools if the fingerprint matches.
// Copies both ways: callers can mutate freely and concurrent saves
// never race caller-owned slices.
func (c *Cache) Get(name, fingerprint string) ([]mcp.Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[name]
	if !ok || e.Fingerprint != fingerprint {
		return nil, false
	}
	return slices.Clone(e.Tools), true
}

// Put stores a COPY of tools and persists (best-effort; silent on failure).
func (c *Cache) Put(name, fingerprint string, tools []mcp.Tool) {
	c.mu.Lock()
	c.data[name] = Entry{Fingerprint: fingerprint, CachedAt: time.Now(), Tools: slices.Clone(tools)}
	c.mu.Unlock()
	c.save()
}

// Drop removes one server (invalidation) and persists.
func (c *Cache) Drop(name string) {
	c.mu.Lock()
	delete(c.data, name)
	c.mu.Unlock()
	c.save()
}

// DropStale removes entries whose fingerprints no longer match current
// config. Called after load/reload.
func (c *Cache) DropStale(current map[string]string) {
	c.mu.Lock()
	changed := false
	for name, e := range c.data {
		if fp, ok := current[name]; !ok || fp != e.Fingerprint {
			delete(c.data, name)
			changed = true
		}
	}
	c.mu.Unlock()
	if changed {
		c.save()
	}
}

func (c *Cache) save() {
	if c.path == "" {
		return
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.RLock()
	f := File{Servers: maps.Clone(c.data)}
	c.mu.RUnlock()
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, c.path)
}
