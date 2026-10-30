package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

// ClientCredentialsMetadata is the subset of auth-server metadata needed for
// the client_credentials grant (RFC 6749 §4.4). mcp-go's OAuthHandler only
// speaks authorization_code (+refresh), so this minimal POST lives here.
type ClientCredentialsMetadata struct {
	Issuer               string `json:"issuer"`
	TokenEndpoint        string `json:"token_endpoint"`
	RegistrationEndpoint string `json:"registration_endpoint"`
	// RFC 8414 advertises an array; TokenEndpointAuthMethod is the resolved
	// choice (basic ONLY when solely offered, else post). Never decoded
	// directly — kept json:"-" so a stray scalar can't shadow the array.
	TokenEndpointAuthMethod string   `json:"-"`
	AuthMethodsSupported    []string `json:"token_endpoint_auth_methods_supported"`
}

// DiscoverClientCredentialsMetadata fetches RFC 8414 metadata from the issuer
// derived from the MCP URL.
func DiscoverClientCredentialsMetadata(ctx context.Context, httpClient *http.Client, issuer string) (*ClientCredentialsMetadata, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, "GET", wellKnown, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("oauth metadata: status %d", resp.StatusCode)
	}
	var md ClientCredentialsMetadata
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return nil, fmt.Errorf("oauth metadata: %w", err)
	}
	if md.TokenEndpoint == "" {
		return nil, fmt.Errorf("oauth metadata: no token_endpoint")
	}
	md.TokenEndpointAuthMethod = ResolveCCAuthMethod(md.AuthMethodsSupported)
	return &md, nil
}

// ResolveCCAuthMethod chooses the client_credentials auth method from the
// RFC 8414 advertised list: client_secret_basic ONLY when solely offered
// (mcp-go parity), else client_secret_post (mcp-go default for silent AS).
func ResolveCCAuthMethod(supported []string) string {
	hasBasic, hasPost := false, false
	for _, m := range supported {
		switch m {
		case "client_secret_basic":
			hasBasic = true
		case "client_secret_post":
			hasPost = true
		}
	}
	if hasBasic && !hasPost {
		return "client_secret_basic"
	}
	return "client_secret_post"
}

// basicEncode applies RFC 6749 §2.3.1 / Appendix B form-encoding to Basic
// credentials: url.QueryEscape is wrong (space → "+" not "%20") and so is
// url.Values.Encode (same). Percent-encode per RFC 3986 unreserved set.
func basicEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ClientCredentialsToken POSTs a client_credentials grant and returns the
// parsed access/refresh/expiry. Auth: secret in body (client_secret_post)
// unless the server metadata lists only client_secret_basic. resource carries
// the RFC 8707 MCP endpoint binding ("" = omit, for lenient servers).
func ClientCredentialsToken(ctx context.Context, httpClient *http.Client, md *ClientCredentialsMetadata, clientID, clientSecret, scope, resource string) (access, refresh string, expiresIn int64, err error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if scope != "" {
		form.Set("scope", scope)
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	form.Set("client_id", clientID)
	method := md.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_post"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", md.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if method == "client_secret_basic" {
		req.SetBasicAuth(basicEncode(clientID), basicEncode(clientSecret))
	} else {
		form.Set("client_secret", clientSecret)
		req.Body = io.NopCloser(strings.NewReader(form.Encode()))
		req.ContentLength = int64(len(form.Encode()))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("client_credentials token: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, fmt.Errorf("client_credentials token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return "", "", 0, fmt.Errorf("client_credentials token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", "", 0, fmt.Errorf("client_credentials token response has no access token")
	}
	return tok.AccessToken, tok.RefreshToken, tok.ExpiresIn, nil
}

// SaveClientCredentialsToken stores a fetched token with issuer binding so
// the mcp-go handler's refresh path honors it.
func SaveClientCredentialsToken(ctx context.Context, store *Store, issuer, access, refresh string, expiresIn int64) error {
	tok := &transport.Token{
		Issuer: issuer, AccessToken: access, RefreshToken: refresh,
		TokenType: "Bearer", ExpiresIn: expiresIn,
	}
	if expiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	}
	return store.SaveToken(ctx, tok)
}
