package config

import (
	"testing"
)

func TestEffectiveGuardTraceForms(t *testing.T) {
	// Pinned: every TOML-writable form from the survey.
	dir := t.TempDir()
	writeTOML(t, dir, `
[daemon]
[daemon.output_guard]
max_bytes = 100
max_lines = 10
[daemon.trace]
enabled = true
[servers.plain]
command = ["x"]
[servers.optout]
command = ["x"]
[servers.optout.output_guard]
enabled = false
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := cfg.Servers["plain"].EffectiveGuard(cfg.Daemon); g == nil || g.MaxBytes != 100 || g.MaxLines != 10 {
		t.Fatalf("plain must inherit daemon guard: %+v", g)
	}
	if g := cfg.Servers["optout"].EffectiveGuard(cfg.Daemon); g != nil {
		t.Fatalf("server opt-out must disable: %+v", g)
	}
	// Pinned: Enabled=false propagates (callers check the flag); zero limits
	// still get defaults.
	tc := cfg.Servers["plain"].EffectiveTrace(cfg.Daemon)
	if tc == nil || !tc.Enabled {
		t.Fatalf("trace must resolve: %+v", tc)
	}
	s := ServerEntry{Trace: &TraceConfig{Enabled: false}}
	if got := s.EffectiveTrace(DaemonConfig{Trace: &TraceConfig{Enabled: true}}); got.Enabled || got.MaxBytes != DefaultTraceMaxBytes {
		t.Fatalf("server Enabled=false must propagate with defaults: %+v", got)
	}
}

func TestEffectiveGuardPrecedence(t *testing.T) {
	d := DaemonConfig{OutputGuard: &OutputGuardConfig{MaxBytes: 100, MaxLines: 10}}
	s := ServerEntry{OutputGuard: &OutputGuardConfig{MaxBytes: 7}}
	if g := s.EffectiveGuard(d); g.MaxBytes != 7 || g.MaxLines != DefaultGuardMaxLines {
		t.Fatalf("server wins, daemon fills zero lines: %+v", g)
	}
	if (ServerEntry{}).EffectiveGuard(DaemonConfig{}) != nil {
		t.Fatal("nil/nil must be off")
	}
	off := false
	if (ServerEntry{}).EffectiveGuard(DaemonConfig{OutputGuard: &OutputGuardConfig{Enabled: &off}}) != nil {
		t.Fatal("daemon enabled=false must be off")
	}
	// Defaults: zero values become adapter-parity defaults.
	g := (&ServerEntry{OutputGuard: &OutputGuardConfig{}}).EffectiveGuard(DaemonConfig{})
	if g.MaxBytes != DefaultGuardMaxBytes || g.MaxLines != DefaultGuardMaxLines {
		t.Fatalf("zero guard defaults wrong: %+v", g)
	}
	tc := (&ServerEntry{Trace: &TraceConfig{Enabled: true}}).EffectiveTrace(DaemonConfig{})
	if tc.MaxBytes != DefaultTraceMaxBytes || tc.MaxEvents != DefaultTraceMaxEvents {
		t.Fatalf("zero trace defaults wrong: %+v", tc)
	}
}

func TestGuardCopyIsolation(t *testing.T) {
	base := &OutputGuardConfig{MaxBytes: 7}
	s := ServerEntry{OutputGuard: base}
	g := s.EffectiveGuard(DaemonConfig{})
	g.MaxBytes = 999
	if base.MaxBytes != 7 {
		t.Fatal("EffectiveGuard must return a copy")
	}
	tb := &TraceConfig{Enabled: true, MaxBytes: 1}
	st := ServerEntry{Trace: tb}
	got := st.EffectiveTrace(DaemonConfig{})
	got.MaxBytes = 999
	if tb.MaxBytes != 1 {
		t.Fatal("EffectiveTrace must return a copy")
	}
}

func TestIdleLifecycleMatrix(t *testing.T) {
	for _, tc := range []struct {
		lc         string
		idle       *int
		dIdle      *int
		wantMode   string
		wantEager  bool
		wantStop   bool
		wantErrSub string
	}{
		{"", nil, nil, "lazy", false, true, ""},
		{"lazy", nil, nil, "lazy", false, true, ""},
		{"eager", nil, nil, "eager", true, false, ""},
		{"keep-alive", nil, nil, "keep-alive", true, false, ""},
		{"lazy-keep-alive", nil, nil, "lazy-keep-alive", false, false, ""},
		{"Eager", nil, nil, "Eager", false, true, "lifecycle must be"},
		{"lazy", new(0), nil, "lazy", false, false, ""},
		{"lazy", new(3), nil, "lazy", false, true, ""},
		{"eager", new(3), nil, "eager", true, true, ""},
		{"lazy", nil, new(0), "lazy", false, false, ""},
		{"eager", nil, new(5), "eager", true, true, ""},
	} {
		s := ServerEntry{Name: "s", Command: []string{"x"}, Lifecycle: tc.lc, IdleTimeout: tc.idle}
		d := DaemonConfig{IdleTimeout: tc.dIdle}
		if m := s.LifecycleMode(); m != tc.wantMode {
			t.Fatalf("%q mode = %q, want %q", tc.lc, m, tc.wantMode)
		}
		if e := s.WantsEagerStart(); e != tc.wantEager {
			t.Fatalf("%q eager = %v, want %v", tc.lc, e, tc.wantEager)
		}
		if st := s.WantsIdleStop(d); st != tc.wantStop {
			t.Fatalf("%q stop = %v, want %v", tc.lc, st, tc.wantStop)
		}
		if msg := validateServer(&s); (tc.wantErrSub == "") != (msg == "") {
			t.Fatalf("%q ConfigError = %q", tc.lc, msg)
		}
	}
	// EffectiveIdleTimeout: server > daemon > hard default.
	if got := (ServerEntry{IdleTimeout: new(3)}).EffectiveIdleTimeout(DaemonConfig{IdleTimeout: new(5)}); got != 3 {
		t.Fatalf("server must win: %d", got)
	}
	if got := (ServerEntry{}).EffectiveIdleTimeout(DaemonConfig{IdleTimeout: new(5)}); got != 5 {
		t.Fatalf("daemon must apply: %d", got)
	}
	if got := (ServerEntry{}).EffectiveIdleTimeout(DaemonConfig{}); got != DefaultIdleTimeoutMin {
		t.Fatalf("hard default = %d", got)
	}
}

func TestIsEnabledSemantics(t *testing.T) {
	if !(ServerEntry{}).IsEnabled() {
		t.Fatal("nil must default true")
	}
	if !(ServerEntry{Enabled: new(true)}).IsEnabled() {
		t.Fatal("explicit true must enable")
	}
	if (ServerEntry{Enabled: new(false)}).IsEnabled() {
		t.Fatal("explicit false must disable")
	}
}
