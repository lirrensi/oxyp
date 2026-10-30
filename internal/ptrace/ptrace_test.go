package ptrace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEmitAndBounds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trace.jsonl")
	tr, err := Open(p, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		tr.Emit(Event{Server: "s", Direction: "out", Method: "tools/call", Outcome: "ok"})
	}
	_ = tr.Close()
	f, _ := os.Open(p)
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line: %v", err)
		}
		if e.Server != "s" || e.Method != "tools/call" || e.Time.IsZero() {
			t.Fatalf("bad event: %+v", e)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("event cap not enforced: %d lines", n)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("trace mode = %o", st.Mode().Perm())
	}
}

func TestNilSafe(t *testing.T) {
	var tr *Tracer
	tr.Emit(Event{Method: "x"}) // must not panic
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}
