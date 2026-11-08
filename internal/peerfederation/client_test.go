package peerfederation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeOwner serves the minimal oxyp REST surface a peer needs.
func fakeOwner(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"missing or wrong bearer token"}`))
			return
		}
		_, _ = w.Write([]byte(`{"pid":1,"startedAt":"x","instance":"owner1","servers":{"db":{"connected":true,"healthy":true}}}`))
	})
	mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"servers":[{"name":"db","description":"Database","origin":"local","connected":true}]}`))
	})
	mux.HandleFunc("GET /peer", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"instance":"owner1","name":"owner","servers":["db"],"count":1}`))
	})
	mux.HandleFunc("GET /servers/{name}/tools", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tools":[{"name":"query","description":"Run SQL"}]}`))
	})
	mux.HandleFunc("GET /servers/{name}/tools/{tool}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"query","description":"Run SQL","inputSchema":{"type":"object","properties":{"q":{"type":"string"}}}}`))
	})
	mux.HandleFunc("POST /servers/{name}/tools/{tool}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(415)
			return
		}
		seenVisited := r.Header.Get(HeaderVisited)
		seenInstance := r.Header.Get(HeaderInstance)
		_, _ = fmt.Fprintf(w, `{"content":[{"type":"text","text":"visited=%s instance=%s"}],"isError":false}`,
			seenVisited, seenInstance)
	})
	mux.HandleFunc("GET /servers/{name}/resources", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resources":[],"instructions":"hi"}`))
	})
	mux.HandleFunc("GET /servers/{name}/prompts", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"prompts":[]}`))
	})
	return httptest.NewServer(mux)
}

func TestClientRoundTrip(t *testing.T) {
	srv := fakeOwner(t, "sekret")
	defer srv.Close()
	c, err := NewClient(srv.URL, "sekret", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithInstance(WithVisited(context.Background(), []string{"a"}), "self")
	h, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Instance != "owner1" {
		t.Fatalf("instance = %+v", h)
	}
	svs, err := c.Servers(ctx)
	if err != nil || len(svs) != 1 || svs[0].Name != "db" {
		t.Fatalf("servers = %+v %v", svs, err)
	}
	pi, err := c.Peer(ctx)
	if err != nil || pi.Instance != "owner1" || pi.Name != "owner" || pi.Count != 1 {
		t.Fatalf("peer = %+v %v", pi, err)
	}
	tools, err := c.Tools(ctx, "db")
	if err != nil || len(tools) != 1 || tools[0].Name != "query" {
		t.Fatalf("tools = %+v %v", tools, err)
	}
	d, err := c.Describe(ctx, "db", "query")
	if err != nil || d.InputSchema["type"] != "object" {
		t.Fatalf("describe = %+v %v", d, err)
	}
	res, err := c.Call(ctx, "db", "query", map[string]any{"q": "1"})
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%v", res.Content)
	if want := "visited=a,self instance=self"; !containsStr(got, want) {
		t.Fatalf("chain not propagated: %s", got)
	}
}

func TestClientAuthFailure(t *testing.T) {
	srv := fakeOwner(t, "sekret")
	defer srv.Close()
	c, err := NewClient(srv.URL+"/ignored?x=1#y", "wrong", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Health(context.Background())
	pe, ok := err.(*PeerError)
	if !ok || pe.Status != 401 {
		t.Fatalf("want 401 PeerError, got %#v", err)
	}
}

func TestBadClientURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "http://", "notaurl"} {
		if _, err := NewClient(u, "", nil, 0); err == nil {
			t.Fatalf("url %q must fail", u)
		}
	}
}

func containsStr(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
