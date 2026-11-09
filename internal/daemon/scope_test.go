package daemon

import (
	"net/url"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
)

func scopeDaemon() *Daemon {
	cfg := &config.Config{
		Servers: map[string]config.ServerEntry{
			"github": {Command: []string{"true"}},
			"linear": {Command: []string{"true"}},
			"gamma":  {Command: []string{"true"}},
		},
		Groups: map[string]config.GroupEntry{
			"coding": {Servers: []string{"github", "linear"}},
		},
	}
	d := New(cfg, testLog())
	return d
}

func TestParseScopeLists(t *testing.T) {
	q := url.Values{}
	q.Add("servers", "a,b")
	q.Add("servers", "c")
	q.Add("exclude", "x, y,,")
	q.Set("group", " coding ")
	s := ParseScope(q)
	if s.Group != "coding" {
		t.Fatalf("group = %q", s.Group)
	}
	if len(s.Allow) != 3 || s.Allow[0] != "a" || s.Allow[2] != "c" {
		t.Fatalf("allow = %v", s.Allow)
	}
	if len(s.Deny) != 2 || s.Deny[0] != "x" {
		t.Fatalf("deny = %v", s.Deny)
	}
	if got := ParseList(""); len(got) != 0 {
		t.Fatalf("empty must be nil, got %v", got)
	}
}

func TestParseArgListShapes(t *testing.T) {
	if got := ParseArgList("a,b"); len(got) != 2 {
		t.Fatalf("string comma: %v", got)
	}
	if got := ParseArgList([]string{"a", "b,c"}); len(got) != 3 {
		t.Fatalf("slice: %v", got)
	}
	if got := ParseArgList([]any{"a", "b,c", 7, nil}); len(got) != 3 {
		t.Fatalf("mixed: %v", got)
	}
	if got := ParseArgList(nil); len(got) != 0 {
		t.Fatalf("nil: %v", got)
	}
}

func TestInScopeAllowExclude(t *testing.T) {
	d := scopeDaemon()
	defer d.Close()

	// empty = all
	if !d.InScope(Scope{}, "github") {
		t.Fatal("empty must allow all")
	}
	// whitelist exact
	s := Scope{Allow: []string{"github"}}
	if !d.InScope(s, "github") || d.InScope(s, "linear") {
		t.Fatal("whitelist exact failed")
	}
	// blacklist exact beats whitelist
	s = Scope{Allow: []string{"github", "linear"}, Deny: []string{"linear"}}
	if !d.InScope(s, "github") || d.InScope(s, "linear") {
		t.Fatal("deny must win")
	}
	// exclude-only = all except
	s = Scope{Deny: []string{"gamma"}}
	if d.InScope(s, "gamma") || !d.InScope(s, "github") {
		t.Fatal("exclude-only failed")
	}
	// globs (same engine as tools): * and ? case-insensitive
	s = Scope{Allow: []string{"GIT*"}}
	if !d.InScope(s, "github") || !d.InScope(s, "GITHUB") {
		t.Fatalf("glob case-insensitive failed")
	}
	s = Scope{Deny: []string{"lin*"}}
	if d.InScope(s, "linear") || !d.InScope(s, "github") {
		t.Fatal("deny glob failed")
	}
	// group ANDs with lists
	s = Scope{Group: "coding", Allow: []string{"github", "gamma"}}
	if !d.InScope(s, "github") || d.InScope(s, "gamma") || d.InScope(s, "linear") {
		t.Fatalf("group AND allow failed")
	}
	// unknown group fails closed
	if d.InScope(Scope{Group: "nope"}, "github") {
		t.Fatal("unknown group must fail closed")
	}
}
