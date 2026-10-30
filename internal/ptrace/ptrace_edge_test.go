package ptrace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLines(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestOpenDefaultsAndErrors(t *testing.T) {
	dir := t.TempDir()
	// zero/negative → internal defaults (write many events, all land).
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		tr.Emit(Event{Server: "s", Method: "m"})
	}
	_ = tr.Close()
	if n := len(readLines(t, p)); n != 10 {
		t.Fatalf("defaults should accept 10 events, got %d lines", n)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("trace mode = %o", st.Mode().Perm())
	}
	// error cases
	if _, err := Open("", 100, 100); err == nil {
		t.Error("empty path must error")
	}
	if _, err := Open(filepath.Join(dir, "no-such-dir", "t.jsonl"), 100, 100); err == nil {
		t.Error("missing parent must error")
	}
	if _, err := Open(dir, 100, 100); err == nil {
		t.Error("directory path must error")
	}
	// negative values also mean defaults.
	p2 := filepath.Join(dir, "t2.jsonl")
	tr2, err := Open(p2, -5, -5)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		tr2.Emit(Event{Server: "s", Method: "m"})
	}
	_ = tr2.Close()
	if n := len(readLines(t, p2)); n != 5 {
		t.Fatalf("negative caps should fall back to defaults, got %d", n)
	}
}

func TestOpenAppendsAndTightensMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(p, []byte(`{"server":"old"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(p, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	tr.Emit(Event{Server: "new", Direction: "out", Method: "m"})
	_ = tr.Close()
	lines := readLines(t, p)
	if len(lines) != 2 || lines[0] != `{"server":"old"}` {
		t.Fatalf("existing content must be preserved: %q", lines)
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil || e.Server != "new" {
		t.Fatalf("appended event bad: %q err=%v", lines[1], err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("existing 0644 must tighten to 0600, got %o", st.Mode().Perm())
	}
}

func TestByteCapOneEventOvershoot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 1, 100000)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		tr.Emit(Event{Server: "s", Direction: "out", Method: "m"})
	}
	_ = tr.Close()
	lines := readLines(t, p)
	// current behavior (gap): the cap is checked before write, so exactly
	// one full event overshoots the 1-byte cap, then everything drops.
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 overshoot line, got %d", len(lines))
	}
	st, _ := os.Stat(p)
	if st.Size() <= 1 {
		t.Fatalf("overshoot line should exceed the 1-byte cap, size=%d", st.Size())
	}
}

func TestExistingSizeCountsTowardByteCap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	// Pre-fill more than maxBytes; Open must accept nothing new.
	if err := os.WriteFile(p, []byte(strings.Repeat("x\n", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	tr, err := Open(p, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	tr.Emit(Event{Server: "s", Method: "m"})
	_ = tr.Close()
	after, _ := os.Stat(p)
	if after.Size() != before.Size() {
		t.Fatalf("size changed %d → %d; existing bytes must count toward cap", before.Size(), after.Size())
	}
}

func TestEventCapExactAndPerSession(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	tr.Emit(Event{Server: "a", Method: "m"})
	tr.Emit(Event{Server: "b", Method: "m"})
	_ = tr.Close()
	lines := readLines(t, p)
	if len(lines) != 1 {
		t.Fatalf("maxEvents=1 must write exactly 1, got %d", len(lines))
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.Server != "a" {
		t.Fatalf("first event must win: %q", lines[0])
	}
	// Reopen resets the event count (per-session, not per-file): one more lands.
	tr2, err := Open(p, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	tr2.Emit(Event{Server: "c", Method: "m"})
	tr2.Emit(Event{Server: "d", Method: "m"})
	_ = tr2.Close()
	if n := len(readLines(t, p)); n != 2 {
		t.Fatalf("reopen resets event cap (current behavior): got %d lines, want 2", n)
	}
}

func TestEmitAfterCloseAndDoubleClose(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	tr.Emit(Event{Server: "a", Method: "m"})
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	tr.Emit(Event{Server: "b", Method: "m"}) // must no-op, not panic
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close must return nil, got %v", err)
	}
	lines := readLines(t, p)
	if len(lines) != 1 {
		t.Fatalf("emit-after-close must be dropped, got %d lines", len(lines))
	}
	// Zero-value Tracer mirrors nil safety: no panic, nil Close.
	var z Tracer
	z.Emit(Event{Method: "x"})
	if err := z.Close(); err != nil {
		t.Fatalf("zero-value Close must be nil: %v", err)
	}
}

func TestTimeOverwrittenUTC(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.FixedZone("X", 3600))
	tr.Emit(Event{Server: "s", Method: "m", Time: sentinel})
	_ = tr.Close()
	lines := readLines(t, p)
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	var e Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	// current behavior: caller Time is always overwritten.
	if e.Time.Equal(sentinel) {
		t.Fatal("caller Time must be overwritten (current behavior)")
	}
	if e.Time.IsZero() {
		t.Fatal("emitted Time must be non-zero")
	}
	if loc := e.Time.Location().String(); loc != "UTC" {
		t.Fatalf("emitted Time location = %q, want UTC", loc)
	}
	if time.Since(e.Time) > time.Minute {
		t.Fatalf("emitted Time not recent: %v", e.Time)
	}
}

func TestJSONLShape(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Note with newline + quote: must stay one physical line.
	tr.Emit(Event{Server: "s", Direction: "out", Method: "tools/call", Note: "line1\nline2\"q\""})
	// Zero Bytes/Latency/Outcome omitted.
	tr.Emit(Event{Server: "s", Direction: "in", Method: "m"})
	_ = tr.Close()
	lines := readLines(t, p)
	if len(lines) != 2 {
		t.Fatalf("newline in Note must not split lines: got %d", len(lines))
	}
	var e0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	for _, k := range []string{"t", "server", "dir", "method", "note"} {
		if _, ok := e0[k]; !ok {
			t.Errorf("line 0 missing key %q: %v", k, e0)
		}
	}
	if e0["note"] != "line1\nline2\"q\"" {
		t.Errorf("note round-trip = %q", e0["note"])
	}
	var e1 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &e1); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bytes", "latencyMs", "outcome"} {
		if _, ok := e1[k]; ok {
			t.Errorf("zero field %q must be omitted: %v", k, e1)
		}
	}
	// Nonzero fields present.
	tr3p := filepath.Join(dir, "t3.jsonl")
	tr3, err := Open(tr3p, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	tr3.Emit(Event{Server: "s", Method: "m", Bytes: 12, LatencyMs: 7, Outcome: "ok"})
	_ = tr3.Close()
	var e2 map[string]any
	raw := readLines(t, tr3p)
	if err := json.Unmarshal([]byte(raw[0]), &e2); err != nil {
		t.Fatal(err)
	}
	if e2["bytes"] != float64(12) || e2["latencyMs"] != float64(7) || e2["outcome"] != "ok" {
		t.Errorf("nonzero fields missing: %v", e2)
	}
}

func TestConcurrentEmitClose(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	tr, err := Open(p, 1<<20, 100000)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 50 {
				tr.Emit(Event{Server: "s", Direction: "out", Method: "m"})
			}
			_ = i
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = tr.Close() // concurrent with emits; must not panic under -race
	}()
	wg.Wait()
	_ = tr.Close()
	lines := readLines(t, p)
	if len(lines) > 400 {
		t.Fatalf("cap exceeded: %d lines", len(lines))
	}
	for _, l := range lines {
		var e Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("torn line: %v", err)
		}
	}
}
