package perms

import (
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
)

func entry(p *config.ToolsPolicy) *config.ServerEntry {
	return &config.ServerEntry{Name: "s", Tools: p}
}

func TestGlobBasics(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"foo", "foo", true},
		{"foo", "FOO", true},
		{"list_*", "list_issues", true},
		{"list_*", "get_issue", false},
		{"*delete*", "my_delete_tool", true},
		{"get_?", "get_x", true},
		{"get_?", "get_xy", false},
		{"***foo", "xxfoo", true}, // collapsed stars
		{"a.c", "a.c", true},
		{"a.c", "axc", false}, // dot is literal
	}
	for _, c := range cases {
		if got := matches(c.pattern, c.value); got != c.want {
			t.Errorf("matches(%q,%q) = %v, want %v", c.pattern, c.value, got, c.want)
		}
	}
}

func TestResolveNilPolicyAllowsAll(t *testing.T) {
	if d := Resolve(entry(nil), "anything"); d.Action != "allow" {
		t.Errorf("nil policy should allow: %+v", d)
	}
	if d := Resolve(entry(&config.ToolsPolicy{}), "anything"); d.Action != "allow" {
		t.Errorf("empty policy should allow: %+v", d)
	}
}

func TestResolveDenyWins(t *testing.T) {
	srv := entry(&config.ToolsPolicy{Allow: []string{"*"}, Deny: []string{"delete_*"}})
	if d := Resolve(srv, "delete_x"); d.Action != "deny" {
		t.Errorf("deny should win: %+v", d)
	}
	if d := Resolve(srv, "list_x"); d.Action != "allow" {
		t.Errorf("non-denied should pass: %+v", d)
	}
	// deny wins even when only allow matches too
	srv2 := entry(&config.ToolsPolicy{Allow: []string{"get_*"}, Deny: []string{"get_secret"}})
	if d := Resolve(srv2, "get_secret"); d.Action != "deny" {
		t.Errorf("exact deny should beat allow glob: %+v", d)
	}
	if d := Resolve(srv2, "get_public"); d.Action != "allow" {
		t.Errorf("allowed glob should pass: %+v", d)
	}
	if d := Resolve(srv2, "other"); d.Action != "deny" {
		t.Errorf("non-allow-listed should deny: %+v", d)
	}
}

func TestVisible(t *testing.T) {
	srv := entry(&config.ToolsPolicy{Deny: []string{"delete_*"}})
	got := Visible(srv, []string{"list_a", "delete_b", "get_c"})
	if len(got) != 2 || got[0] != "list_a" || got[1] != "get_c" {
		t.Fatalf("Visible = %v", got)
	}
}
