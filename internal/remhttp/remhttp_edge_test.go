package remhttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSameOriginKeepsPost301_302_303(t *testing.T) {
	for _, code := range []int{301, 302, 303} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var gotMethod, gotBody, gotCT, gotCustom string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redir" {
					http.Redirect(w, r, "/mcp", code)
					return
				}
				gotMethod = r.Method
				b, _ := io.ReadAll(r.Body)
				gotBody, gotCT, gotCustom = string(b), r.Header.Get("Content-Type"), r.Header.Get("X-Custom")
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
			if gotCustom != "" {
				t.Fatalf("X-Custom dropped: %q (rebuild keeps prev headers)", gotCustom)
			}
			_ = gotCT
		})
	}
}

func TestSameOriginPostKeepsHeadersAndBody(t *testing.T) {
	var gotCustom, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/mcp", http.StatusFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCustom, gotCT = string(b), r.Header.Get("X-Custom"), r.Header.Get("Content-Type")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := NewClient(5*time.Second, TransportTLS{})
	req, _ := http.NewRequest("POST", srv.URL+"/redir", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom", "keep-me")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotBody != `{"a":1}` || gotCustom != "keep-me" || gotCT != "application/json" {
		t.Fatalf("same-origin rebuild: body=%q custom=%q ct=%q", gotBody, gotCustom, gotCT)
	}
}

func TestRedirectCapBoundary(t *testing.T) {
	mk := func(n int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/done" {
				w.Write([]byte("ok"))
				return
			}
			rest := strings.TrimPrefix(r.URL.Path, "/hop/")
			num := 0
			for _, c := range rest {
				num = num*10 + int(c-'0')
			}
			if num >= n {
				http.Redirect(w, r, "/done", http.StatusFound)
			} else {
				http.Redirect(w, r, "/hop/"+itoa(num+1), http.StatusFound)
			}
		}))
	}
	c := NewClient(5*time.Second, TransportTLS{})
	// checkRedirect fires per follow with len(via)=1..k and fails at 5, so
	// 4 follows (mk(3): /hop/0..2 redirect + /hop/3 -> /done) succeed...
	srv3 := mk(3)
	defer srv3.Close()
	resp, err := c.Get(srv3.URL + "/hop/0")
	if err != nil {
		t.Fatalf("4 follows: %v", err)
	}
	resp.Body.Close()
	// ...while 5 follows (mk(4)) hit the cap with the exact error.
	srv4 := mk(4)
	defer srv4.Close()
	_, err = c.Get(srv4.URL + "/hop/0")
	if err == nil || !strings.Contains(err.Error(), "oxyp: too many redirects (5)") {
		t.Fatalf("cap: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestCrossOriginProtocolHeadersSurvive(t *testing.T) {
	got := map[string]string{}
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"Authorization", "X-Api-Key", "Cookie", "X-Custom", "Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Referer"} {
			got[h] = r.Header.Get(h)
		}
		got["Host"] = r.Host
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
	req.Header.Set("X-Api-Key", "k")
	req.Header.Set("Cookie", "c=1")
	req.Header.Set("X-Custom", "x")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", "sess")
	req.Header.Set("MCP-Protocol-Version", "v1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, h := range []string{"Authorization", "X-Api-Key", "Cookie", "X-Custom"} {
		if got[h] != "" {
			t.Fatalf("cross-origin leak %s=%q", h, got[h])
		}
	}
	// Protocol headers survive. Note: Go's copier drops Content-Type on
	// POST→GET body-strip redirects (current behavior: Content-Type is empty
	// after the cross-origin 302 rewrite).
	for _, h := range []string{"Accept", "Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if got[h] == "" {
			t.Fatalf("protocol header %s stripped", h)
		}
	}
	var _ = got["Content-Type"]
	var _ = got["Referer"] // Referer is managed by Go, not the policy
}

func TestCrossOriginByPortAndScheme(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer second.Close()
	// Different port on the same loopback IP is cross-origin (Host includes port).
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/x", http.StatusFound)
	}))
	defer first.Close()
	c := NewClient(5*time.Second, TransportTLS{})
	req, _ := http.NewRequest("GET", first.URL+"/y", nil)
	req.Header.Set("Authorization", "Bearer s")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "" {
		t.Fatalf("port change must strip Authorization: %q", b)
	}
	// http→https redirect also strips (scheme part of sameOrigin).
	tlssrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer tlssrv.Close()
	mid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tlssrv.URL+"/x", http.StatusFound)
	}))
	defer mid.Close()
	cc := NewClient(5*time.Second, TransportTLS{Insecure: true})
	req2, _ := http.NewRequest("GET", mid.URL+"/y", nil)
	req2.Header.Set("Authorization", "Bearer s")
	resp2, err := cc.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(b2) != "" {
		t.Fatalf("scheme change must strip Authorization: %q", b2)
	}
}

func TestSameOriginPOSTWithoutGetBody(t *testing.T) {
	// A body without GetBody + same-origin 302: no panic; current behavior
	// is the POST is re-sent with an empty body (checkRedirect only restores
	// bodies when prev.GetBody != nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/mcp", http.StatusFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		w.Write([]byte(r.Method + ":" + string(b)))
	}))
	defer srv.Close()
	c := NewClient(5*time.Second, TransportTLS{})
	req, _ := http.NewRequest("POST", srv.URL+"/redir", &noGetBodyReader{data: []byte("hello")})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "POST:" {
		t.Fatalf("want POST with empty body, got %q", b)
	}
}

type noGetBodyReader struct {
	data []byte
	off  int
}

func (r *noGetBodyReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func TestRedirect307WithoutGetBody(t *testing.T) {
	// Go refuses to follow 307 when GetBody is nil: the 307 response is
	// returned as-is, no error (client.go redirectBehavior).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/mcp", http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("arrived"))
	}))
	defer srv.Close()
	c := NewClient(5*time.Second, TransportTLS{})
	req, _ := http.NewRequest("POST", srv.URL+"/redir", &noGetBodyReader{data: []byte("hello")})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("want 307 returned, got %d", resp.StatusCode)
	}
}

func TestClientTimeoutCoversBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()
	c := NewClient(50*time.Millisecond, TransportTLS{})
	start := time.Now()
	resp, err := c.Get(srv.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("timeout did not bound the body read: %v", time.Since(start))
	}
}

func TestZeroTimeoutStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := NewClient(0, TransportTLS{})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Fatalf("body: %q", b)
	}
}

func TestTransportSettings(t *testing.T) {
	c := NewClient(7*time.Second, TransportTLS{})
	if c.Timeout != 7*time.Second {
		t.Fatalf("timeout: %v", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type: %T", c.Transport)
	}
	if !tr.ForceAttemptHTTP2 || tr.MaxIdleConns != 16 ||
		tr.IdleConnTimeout != 90*time.Second || tr.TLSHandshakeTimeout != 10*time.Second ||
		tr.ExpectContinueTimeout != time.Second || tr.Proxy == nil {
		t.Fatalf("transport fields: %+v", tr)
	}
}
