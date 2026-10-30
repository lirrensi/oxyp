package remhttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSameOriginKeepsPost(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/mcp", http.StatusFound) // 302
			return
		}
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewClient(5*time.Second, TransportTLS{})
	resp, err := c.Post(srv.URL+"/redir", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotMethod != "POST" || gotBody != `{"a":1}` {
		t.Fatalf("redirect lost POST: method=%s body=%q", gotMethod, gotBody)
	}
}

func TestCrossOriginStripsAllButProtocol(t *testing.T) {
	var gotAuth, gotTenant, gotCustom string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Tenant")
		gotCustom = r.Header.Get("X-Custom")
		w.Write([]byte("ok"))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/x", http.StatusFound)
	}))
	defer first.Close()

	c := NewClient(5*time.Second, TransportTLS{})
	req, _ := http.NewRequest("POST", first.URL+"/redir", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("X-Tenant", "acme")
	req.Header.Set("X-Custom", "x")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "" || gotTenant != "" || gotCustom != "" {
		t.Fatalf("cross-origin leak: auth=%q tenant=%q custom=%q", gotAuth, gotTenant, gotCustom)
	}
}

func TestRedirectCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()
	c := NewClient(5*time.Second, TransportTLS{})
	_, err := c.Post(srv.URL+"/loop", "application/json", strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("want redirect-cap error")
	}
}

func TestValidateTLS(t *testing.T) {
	if got := ValidateTLS("s", "", "cert.pem", ""); got == "" {
		t.Fatal("want cert-without-key error")
	}
	if got := ValidateTLS("s", "/nonexistent-ca.pem", "", ""); got == "" {
		t.Fatal("want missing-ca error")
	}
	if got := ValidateTLS("s", "", "", ""); got != "" {
		t.Fatalf("empty TLS should validate: %s", got)
	}
}
