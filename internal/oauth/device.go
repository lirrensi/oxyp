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
)

// Device flow (RFC 8628), ported from hermes tools/mcp_oauth_device.py.
// No browser, no callback socket: the user opens verification_uri on ANY
// device and types user_code. Polling state machine mirrors hermes:
// authorization_pending → keep polling; slow_down → interval+5s;
// access_denied/expired_token → fail; transport timeout → interval×2.

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// DeviceMetadata is the subset of auth-server metadata we need.
type DeviceMetadata struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	RegistrationEndpoint        string `json:"registration_endpoint"`
}

// DeviceAuthorization is the device_authorize response.
type DeviceAuthorization struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
}

// DeviceClient holds DCR credentials for the device flow.
type DeviceClient struct {
	ClientID     string
	ClientSecret string
}

// DiscoverDeviceMetadata fetches RFC 8414 metadata and requires a device endpoint.
func DiscoverDeviceMetadata(ctx context.Context, httpClient *http.Client, issuer string) (*DeviceMetadata, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, "GET", wellKnown, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("device metadata: status %d", resp.StatusCode)
	}
	var md DeviceMetadata
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return nil, fmt.Errorf("device metadata: %w", err)
	}
	if md.DeviceAuthorizationEndpoint == "" {
		return nil, fmt.Errorf("server does not advertise device authorization; use flow=browser if supported")
	}
	if md.Issuer == "" {
		return nil, fmt.Errorf("device metadata: missing issuer (needed for token refresh binding)")
	}
	if md.TokenEndpoint == "" {
		return nil, fmt.Errorf("device metadata: missing token_endpoint")
	}
	return &md, nil
}

// RegisterDeviceClient performs DCR with device grant types, or uses configured creds.
func RegisterDeviceClient(ctx context.Context, httpClient *http.Client, md *DeviceMetadata, clientID, clientSecret string) (*DeviceClient, error) {
	if clientID != "" {
		return &DeviceClient{ClientID: clientID, ClientSecret: clientSecret}, nil
	}
	if md.RegistrationEndpoint == "" {
		return nil, fmt.Errorf("server has no registration endpoint; configure oauth.client_id")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name": "oxyp",
		"grant_types": []string{deviceGrant, "refresh_token"},
		"response_types": []string{},
		// Match what we actually send: the issued secret goes out via
		// client_secret_post (applyClientAuth). Registering "none" while
		// sending a secret breaks strict servers. No secret issued =
		// nothing sent, which degrades to public-equivalent anyway.
		"token_endpoint_auth_method": "client_secret_post",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", md.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device DCR: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("device DCR: status %d: %s", resp.StatusCode, b)
	}
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, fmt.Errorf("device DCR: %w", err)
	}
	if reg.ClientID == "" {
		return nil, fmt.Errorf("device DCR: no client_id returned")
	}
	return &DeviceClient{ClientID: reg.ClientID, ClientSecret: reg.ClientSecret}, nil
}

// RequestDeviceCode starts device authorization. resource carries the
// RFC 8707 MCP endpoint binding ("" = omit, for lenient servers).
func RequestDeviceCode(ctx context.Context, httpClient *http.Client, md *DeviceMetadata, dc *DeviceClient, scope, resource string) (*DeviceAuthorization, error) {
	form := url.Values{
		"client_id": {dc.ClientID},
	}
	if scope != "" {
		form.Set("scope", scope)
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", md.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	applyClientAuth(req, dc)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("device authorize: status %d: %s", resp.StatusCode, b)
	}
	var da DeviceAuthorization
	if err := json.NewDecoder(resp.Body).Decode(&da); err != nil {
		return nil, fmt.Errorf("device authorize: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return nil, fmt.Errorf("device authorization is missing device_code/user_code/verification_uri")
	}
	if da.Interval <= 0 {
		da.Interval = 5
	}
	if da.ExpiresIn <= 0 {
		da.ExpiresIn = 300
	}
	return &da, nil
}

// PollDeviceToken polls the token endpoint until approval, denial, or deadline.
// showCode is called once with the URI + user code for display. resource is
// the RFC 8707 MCP endpoint binding, sent on every token poll ("" = omit).
func PollDeviceToken(ctx context.Context, httpClient *http.Client, md *DeviceMetadata, dc *DeviceClient, da *DeviceAuthorization, timeout time.Duration, resource string, showCode func(uri, code string)) (access, refresh string, expiresIn int64, err error) {
	showCode(da.VerificationURI, da.UserCode)
	deadline := time.Now().Add(minDur(time.Duration(da.ExpiresIn)*time.Second, timeout))
	interval := time.Duration(da.Interval) * time.Second
	for {
		remaining := time.Until(deadline)
		if remaining <= interval {
			return "", "", 0, fmt.Errorf("device authorization expired before approval; run login again")
		}
		select {
		case <-ctx.Done():
			return "", "", 0, ctx.Err()
		case <-time.After(interval):
		}
		form := url.Values{
			"grant_type":  {deviceGrant},
			"device_code": {da.DeviceCode},
			"client_id":   {dc.ClientID},
		}
		if resource != "" {
			form.Set("resource", resource)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", md.TokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return "", "", 0, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		applyClientAuth(req, dc)
		resp, err := httpClient.Do(req)
		if err != nil {
			// RFC 8628: reduce polling frequency after transport timeouts.
			interval *= 2
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var tok struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int64  `json:"expires_in"`
			}
			if err := json.Unmarshal(b, &tok); err != nil {
				return "", "", 0, fmt.Errorf("device token: %w", err)
			}
			if tok.AccessToken == "" {
				return "", "", 0, fmt.Errorf("device token response has no access token")
			}
			return tok.AccessToken, tok.RefreshToken, tok.ExpiresIn, nil
		}
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &errBody)
		switch errBody.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied", "expired_token":
			return "", "", 0, fmt.Errorf("device authorization failed: %s", errBody.Error)
		default:
			return "", "", 0, fmt.Errorf("device authorization failed: HTTP %d", resp.StatusCode)
		}
	}
}

// applyClientAuth adds client_secret_post auth when we have a secret.
func applyClientAuth(req *http.Request, dc *DeviceClient) {
	if dc.ClientSecret != "" {
		req.SetBasicAuth(dc.ClientID, dc.ClientSecret)
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
