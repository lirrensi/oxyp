// Package remhttp builds hardened http.Clients for remote MCP servers.
//
// Neither donor (lazy-mcp: fetch default; mcpd: nothing) sets a redirect
// policy. Ours:
//   - follow at most 5 redirects (Go default is 10);
//   - on 301/302/303, Go rewrites POST→GET and drops the body — for MCP we
//     rebuild the POST with the original body when staying same-origin;
//   - cross-origin: strip ALL headers except the MCP protocol set
//     (Go strips only Authorization; X-Api-Key etc would leak).
package remhttp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// NewClient returns an http.Client with oxyp's redirect policy and timeout.
func NewClient(timeout time.Duration, tls TransportTLS) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:       tls.config(),
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: checkRedirect,
	}
}

// protocolHeaders survive a cross-origin redirect. Everything else is dropped.
var protocolHeaders = []string{"Accept", "Content-Type", "Mcp-Session-Id", "MCP-Protocol-Version"}

// checkRedirect enforces: max 5 hops; same-origin 301/302/303 keeps POST+body;
// cross-origin strips all non-protocol headers.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("oxyp: too many redirects (5)")
	}
	prev := via[len(via)-1]
	sameOrigin := req.URL.Scheme == prev.URL.Scheme && req.URL.Host == prev.URL.Host
	if !sameOrigin {
		keep := map[string][]string{}
		for _, h := range protocolHeaders {
			if v, ok := req.Header[http.CanonicalHeaderKey(h)]; ok {
				keep[http.CanonicalHeaderKey(h)] = v
			}
		}
		for k := range req.Header {
			delete(req.Header, k)
		}
		for k, v := range keep {
			req.Header[k] = v
		}
		return nil
	}
	// Same origin: Go already rewrote POST→GET for 301/302/303. Rebuild POST.
	if prev.Method == http.MethodPost && req.Method == http.MethodGet {
		req.Method = http.MethodPost
		if prev.GetBody != nil {
			body, err := prev.GetBody()
			if err != nil {
				return err
			}
			b, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			_ = body.Close()
			req.Body = io.NopCloser(bytes.NewReader(b))
			req.ContentLength = int64(len(b))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(b)), nil
			}
		}
		for k, v := range prev.Header {
			if _, ok := req.Header[k]; !ok {
				req.Header[k] = v
			}
		}
	}
	return nil
}
