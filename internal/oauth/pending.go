package oauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// PendingFlow is a started browser flow awaiting manual completion.
// Persisted so `login --complete` works across processes (SSH without
// loopback forward): the printing login saves it, the completing login
// loads it. Same dir/key scheme as token Store.
type PendingFlow struct {
	State        string `json:"state"`
	Verifier     string `json:"verifier"`
	RedirectURI  string `json:"redirect_uri"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AuthMethod   string `json:"auth_method,omitempty"`
	Issuer       string `json:"issuer,omitempty"`
}

func pendingPath(dir, key string) string { return filepath.Join(dir, key+".pending.json") }

// SavePending writes the pending flow (0600, like tokens).
func SavePending(dir, key string, p PendingFlow) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := pendingPath(dir, key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pendingPath(dir, key))
}

// LoadPending reads a pending flow; nil, nil when none exists.
func LoadPending(ctx context.Context, dir, key string) (*PendingFlow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(pendingPath(dir, key))
	if err != nil {
		return nil, nil
	}
	var p PendingFlow
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, nil
	}
	if p.State == "" || p.Verifier == "" {
		return nil, nil
	}
	return &p, nil
}

// ClearPending removes a pending flow (best-effort).
func ClearPending(dir, key string) {
	_ = os.Remove(pendingPath(dir, key))
}
