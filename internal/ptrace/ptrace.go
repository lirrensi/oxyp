// Package ptrace emits metadata-only protocol traces as JSONL.
// One event per line: timestamp, server, direction, method, size class,
// latency, outcome. NEVER payloads, tool args/results, auth, or URLs.
package ptrace

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event is one trace line.
type Event struct {
	Time      time.Time `json:"t"`
	Server    string    `json:"server"`
	Direction string    `json:"dir"` // out (daemon→downstream) or in
	Method    string    `json:"method"`
	Bytes     int       `json:"bytes,omitempty"`
	LatencyMs int64     `json:"latencyMs,omitempty"`
	Outcome   string    `json:"outcome,omitempty"` // ok | error:<class>
	Note      string    `json:"note,omitempty"`    // short, metadata-only
}

// Tracer appends events to a bounded JSONL file. Zero value = disabled.
type Tracer struct {
	mu     sync.Mutex
	f      *os.File
	events int
	maxEv  int
	maxBy  int64
	wrote  int64
}

// Open creates the trace file (0600). maxBytes/maxEvents <= 0 = defaults.
func Open(path string, maxBytes int, maxEvents int) (*Tracer, error) {
	if maxBytes <= 0 {
		maxBytes = 262144
	}
	if maxEvents <= 0 {
		maxEvents = 10000
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	st, _ := f.Stat()
	return &Tracer{f: f, maxEv: maxEvents, maxBy: int64(maxBytes), wrote: st.Size()}, nil
}

// Emit appends one event. Silently drops once either bound is hit
// (bounded, never grows the disk).
func (t *Tracer) Emit(e Event) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil || t.events >= t.maxEv || t.wrote >= t.maxBy {
		return
	}
	e.Time = time.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	n, _ := t.f.Write(b)
	t.wrote += int64(n)
	t.events++
}

// Close flushes and closes the file.
func (t *Tracer) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	return err
}
