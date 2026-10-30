package daemon

import (
	"sort"
	"testing"

	"oxyp/internal/config"
)

func strSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Entry must expose a live, unshared copy. The assignment pins CURRENT
// (shallow-copy) behavior: nested maps/slices/pointers are shared with the
// live entry. If a future fix deep-copies, update this test to assert
// isolation instead.
func TestEntryCurrentSharingBehavior(t *testing.T) {
	inherit := true
	enabled := true
	idle := 5
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"s": {
			Description: "d",
			Command:     []string{"prog", "a1"},
			Headers:     map[string]string{"X-A": "1"},
			Env:         map[string]string{"E": "1"},
			InheritEnv:  &inherit,
			Enabled:     &enabled,
			IdleTimeout: &idle,
			OAuth:       &config.OAuthConfig{ClientID: "cid"},
		},
	}}, testLog())
	defer d.Close()

	e := d.Entry("s")
	if e == nil {
		t.Fatal("want entry")
	}
	if e.Command[0] != "prog" || e.Headers["X-A"] != "1" || e.Env["E"] != "1" {
		t.Fatalf("bad copy: %+v", e)
	}
	if e.InheritEnv == nil || !*e.InheritEnv || e.Enabled == nil || !*e.Enabled {
		t.Fatalf("pointer fields must be carried: %+v", e)
	}
	if e.OAuth == nil || e.OAuth.ClientID != "cid" {
		t.Fatalf("oauth must be carried: %+v", e)
	}
	// Map key wins over the struct's Name field.
	if e.Name != "s" {
		t.Fatalf("Name should be the map key, got %q", e.Name)
	}
	// Missing server returns nil.
	if d.Entry("nope") != nil {
		t.Fatal("missing server must return nil")
	}

	// Deep copy: nested writes must NOT alias live state.
	e2 := d.Entry("s")
	e2.Command[0] = "MUT"
	e2.Headers["X-A"] = "MUT"
	e2.Env["E"] = "MUT"
	*e2.InheritEnv = false
	*e2.IdleTimeout = 99
	e2.OAuth.ClientID = "MUT"
	live := d.Entry("s")
	if live.Command[0] == "MUT" || live.Headers["X-A"] == "MUT" ||
		live.Env["E"] == "MUT" || !*live.InheritEnv ||
		*live.IdleTimeout == 99 || live.OAuth.ClientID == "MUT" {
		t.Fatalf("Entry must deep-copy; live state mutated: %+v", live)
	}
}

func TestNamesExcludesDisabled(t *testing.T) {
	f := false
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"a":   {Command: []string{"true"}},
		"b":   {Command: []string{"true"}},
		"z":   {Command: []string{"true"}},
		"off": {Command: []string{"true"}, Enabled: &f},
	}}, testLog())
	defer d.Close()
	names := d.Names()
	if len(names) != 3 {
		t.Fatalf("disabled must be excluded: %v", names)
	}
	sort.Strings(names)
	if !strSlicesEqual(names, []string{"a", "b", "z"}) {
		t.Fatalf("want [a b z], got %v", names)
	}
}

func TestNewMapKeyWinsAndNameOverwrite(t *testing.T) {
	d := New(&config.Config{Servers: map[string]config.ServerEntry{
		"real": {Name: "wrong", Command: []string{"true"}},
	}}, testLog())
	defer d.Close()
	if e := d.Entry("real"); e == nil || e.Name != "real" {
		t.Fatalf("map key must win: %+v", e)
	}
	// Disabled entries are dropped entirely.
	f := false
	d2 := New(&config.Config{Servers: map[string]config.ServerEntry{
		"off": {Command: []string{"true"}, Enabled: &f},
	}}, testLog())
	defer d2.Close()
	if len(d2.Names()) != 0 || d2.Entry("off") != nil {
		t.Fatalf("disabled entries must be dropped: %v", d2.Names())
	}
}

func TestDaemonConfigCopy(t *testing.T) {
	idle := 7
	d := New(&config.Config{
		Daemon:  config.DaemonConfig{IdleTimeout: &idle},
		Servers: map[string]config.ServerEntry{"s": {Command: []string{"true"}}},
	}, testLog())
	defer d.Close()
	got := d.DaemonConfig()
	if got.IdleTimeout == nil || *got.IdleTimeout != 7 {
		t.Fatalf("want idle 7: %+v", got)
	}
	// Deep copy: mutating the returned struct must not affect the daemon.
	*got.IdleTimeout = 99
	if again := d.DaemonConfig(); *again.IdleTimeout != 7 {
		t.Fatalf("DaemonConfig must deep-copy IdleTimeout: %+v", again)
	}
}

func TestAuthNeededErrorBranches(t *testing.T) {
	noURL := (&AuthNeededError{Server: "s"}).Error()
	if noURL == "" || !contains(noURL, "oxyp login s") {
		t.Fatalf("no-URL branch must carry login hint: %q", noURL)
	}
	withURL := (&AuthNeededError{Server: "s", URL: "https://as/cb"}).Error()
	if !contains(withURL, "https://as/cb") || !contains(withURL, `"s"`) {
		t.Fatalf("URL branch must carry server + URL: %q", withURL)
	}
}
