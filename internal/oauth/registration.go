package oauth

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/client/transport"
)

// ClientReg is a persisted Dynamic Client Registration: the client_id/secret,
// token-endpoint auth method, and credentials issuer mcp-go learned during
// GetAuthorizationURL. Token files alone are not enough — after a daemon
// restart or in a fresh `login --complete` process the rebuilt handler would
// exchange/refresh with empty credentials and strict IdPs reject it with
// invalid_client. Stored as a sidecar next to the token file; additive, no
// migration of existing token files.
type ClientReg struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	AuthMethod   string `json:"auth_method,omitempty"`
	Issuer       string `json:"issuer,omitempty"`
}

func clientRegPath(dir, key string) string { return filepath.Join(dir, key+".client.json") }

// SnapshotClientReg reads post-DCR credentials out of a handler. Call after
// RegisterClient + GetAuthorizationURL; empty ClientID means neither DCR nor
// static credentials produced an id, and there is nothing to persist.
func SnapshotClientReg(h *transport.OAuthHandler) ClientReg {
	return ClientReg{
		ClientID:     h.GetClientID(),
		ClientSecret: h.GetClientSecret(),
		AuthMethod:   h.GetTokenEndpointAuthMethod(),
		Issuer:       h.GetCredentialsIssuer(),
	}
}

// SaveClientReg writes the sidecar (0600, like tokens). Best-effort callers
// may ignore the error; a missing sidecar just means the next process starts
// from config credentials as before.
func SaveClientReg(dir, key string, reg ClientReg) error {
	if reg.ClientID == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(reg)
	if err != nil {
		return err
	}
	tmp := clientRegPath(dir, key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, clientRegPath(dir, key))
}

// LoadClientReg reads the sidecar; nil, nil when none exists (fresh login or
// a static-client flow that never registered). Callers fall back to config
// credentials.
func LoadClientReg(dir, key string) (*ClientReg, error) {
	b, err := os.ReadFile(clientRegPath(dir, key))
	if err != nil {
		return nil, nil
	}
	var reg ClientReg
	if err := json.Unmarshal(b, &reg); err != nil {
		return nil, nil
	}
	if reg.ClientID == "" {
		return nil, nil
	}
	return &reg, nil
}

// ClearClientReg removes the sidecar (best-effort).
func ClearClientReg(dir, key string) {
	_ = os.Remove(clientRegPath(dir, key))
}

// ApplyClientReg fills empty handler-config credentials from a saved
// registration. Configured values always win; the sidecar only backfills.
func ApplyClientReg(ocfg *transport.OAuthConfig, reg *ClientReg) {
	if reg == nil {
		return
	}
	if ocfg.ClientID == "" {
		ocfg.ClientID = reg.ClientID
	}
	if ocfg.ClientSecret == "" {
		ocfg.ClientSecret = reg.ClientSecret
	}
	if ocfg.CredentialsIssuer == "" {
		ocfg.CredentialsIssuer = reg.Issuer
	}
	if ocfg.TokenEndpointAuthMethod == "" {
		ocfg.TokenEndpointAuthMethod = reg.AuthMethod
	}
}
