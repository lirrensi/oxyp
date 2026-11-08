package peerfederation

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// identityFile is the persistent daemon id, next to oxyp.toml.
const identityFile = "identity.json"

// identity is the on-disk shape: just the id (name lives in oxyp.toml).
type identity struct {
	ID string `json:"id"`
}

// LoadOrCreateIdentity reads <dir>/identity.json, creating it (0600) with a
// fresh UUID on first run. The id survives restarts: peers recognize us, and
// /peer handshakes stay stable. Corrupt/empty file = fresh id (fail open is
// safe: worst case peers see a stranger and resync).
func LoadOrCreateIdentity(dir string) (string, error) {
	path := filepath.Join(dir, identityFile)
	if b, err := os.ReadFile(path); err == nil {
		var v identity
		if jerr := json.Unmarshal(b, &v); jerr == nil && v.ID != "" {
			if _, perr := uuid.Parse(v.ID); perr == nil {
				return v.ID, nil
			}
		}
	}
	id := uuid.NewString()
	b, err := json.Marshal(identity{ID: id})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".identity-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return id, nil
}

// ShortID renders the first 8 chars of an id for logs/diagnostics.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
