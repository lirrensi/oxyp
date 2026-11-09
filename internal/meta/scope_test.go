package meta

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
)

func scopeTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Servers: map[string]config.ServerEntry{
			"github": {Command: []string{"true"}, Description: "gh"},
			"linear": {Command: []string{"true"}, Description: "lin"},
			"gamma":  {Command: []string{"true"}, Description: "g"},
		},
		Groups: map[string]config.GroupEntry{
			"coding": {Servers: []string{"github", "linear"}},
		},
	}
	d := daemon.New(cfg, nil)
	t.Cleanup(d.Close)
	return New(d, func() (added, removed, modified []string, err error) {
		return nil, nil, nil, nil
	}, "", "127.0.0.1:8379")
}

func scopeGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, r)
	return rr
}

func scopeServers(t *testing.T, s *Server, path string) []string {
	t.Helper()
	rr := scopeGet(t, s, path)
	if rr.Code != 200 {
		t.Fatalf("%s -> %d %s", path, rr.Code, rr.Body.String())
	}
	var v struct {
		Servers []struct {
			Name string `json:"name"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range v.Servers {
		out = append(out, x.Name)
	}
	return out
}

func TestScopeFiltersServersREST(t *testing.T) {
	s := scopeTestServer(t)

	if got := scopeServers(t, s, "/servers"); len(got) != 3 {
		t.Fatalf("all = %v", got)
	}
	if got := scopeServers(t, s, "/servers?servers=github,linear"); len(got) != 2 {
		t.Fatalf("whitelist = %v", got)
	}
	if got := scopeServers(t, s, "/servers?exclude=gamma"); len(got) != 2 {
		t.Fatalf("exclude = %v", got)
	}
	if got := scopeServers(t, s, "/servers?servers=git*&exclude=gamma"); len(got) != 1 || got[0] != "github" {
		t.Fatalf("glob+deny = %v", got)
	}
	if got := scopeServers(t, s, "/servers?group=coding&exclude=linear"); len(got) != 1 || got[0] != "github" {
		t.Fatalf("group AND exclude = %v", got)
	}
	if rr := scopeGet(t, s, "/servers?group=nope"); rr.Code != 404 {
		t.Fatalf("unknown group must 404, got %d", rr.Code)
	}
	if rr := scopeGet(t, s, "/servers/gamma/tools?servers=github"); rr.Code != 403 {
		t.Fatalf("outside scope must 403, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := scopeGet(t, s, "/servers/gamma/tools?group=coding"); rr.Code != 403 {
		t.Fatalf("outside group must 403, got %d", rr.Code)
	}
}
