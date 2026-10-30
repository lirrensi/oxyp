package oauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestClientRegFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oauth")
	key := "srv"
	reg := ClientReg{ClientID: "id1", ClientSecret: "sec", AuthMethod: "post", Issuer: "https://iss"}
	if err := SaveClientReg(dir, key, reg); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, key+".client.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode: %v %v", st, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", st, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("tmp leftovers: %v", leftovers)
	}
	got, err := LoadClientReg(dir, key)
	if err != nil || got == nil || *got != reg {
		t.Fatalf("Load: %+v %v", got, err)
	}
	// Per-key isolation.
	other, err := LoadClientReg(dir, "nope")
	if err != nil || other != nil {
		t.Fatalf("isolation: %+v %v", other, err)
	}
	ClearClientReg(dir, key)
	if got, _ := LoadClientReg(dir, key); got != nil {
		t.Fatal("Clear did not remove")
	}
	ClearClientReg(dir, key) // missing: no panic
	// Empty-ID save is a no-op (current behavior: writes nothing).
	if err := SaveClientReg(dir, "empty", ClientReg{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "empty.client.json")); !os.IsNotExist(err) {
		t.Fatal("empty ClientID must write nothing")
	}
	// Corrupt JSON and empty-ID JSON both load as nil,nil.
	if err := os.WriteFile(filepath.Join(dir, key+".client.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadClientReg(dir, key); got != nil || err != nil {
		t.Fatalf("corrupt: %+v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".client.json"), []byte(`{"client_id":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadClientReg(dir, key); got != nil || err != nil {
		t.Fatalf("empty ID: %+v %v", got, err)
	}
}

func TestApplyClientRegPrecedence(t *testing.T) {
	ApplyClientReg(nil, nil) // must not panic on double nil
	var ocfg transport.OAuthConfig
	ApplyClientReg(&ocfg, nil)
	if ocfg.ClientID != "" || ocfg.ClientSecret != "" || ocfg.CredentialsIssuer != "" || ocfg.TokenEndpointAuthMethod != "" {
		t.Fatal("nil reg must be no-op")
	}
	reg := &ClientReg{ClientID: "r-id", ClientSecret: "r-sec", Issuer: "r-iss", AuthMethod: "r-method"}
	ocfg = transport.OAuthConfig{}
	ApplyClientReg(&ocfg, reg)
	if ocfg.ClientID != "r-id" || ocfg.ClientSecret != "r-sec" ||
		ocfg.CredentialsIssuer != "r-iss" || ocfg.TokenEndpointAuthMethod != "r-method" {
		t.Fatalf("backfill: %+v", ocfg)
	}
	// Configured values win, per field.
	ocfg = transport.OAuthConfig{ClientID: "cfg-id", CredentialsIssuer: "cfg-iss"}
	ApplyClientReg(&ocfg, reg)
	if ocfg.ClientID != "cfg-id" || ocfg.CredentialsIssuer != "cfg-iss" {
		t.Fatalf("precedence: %+v", ocfg)
	}
	if ocfg.ClientSecret != "r-sec" || ocfg.TokenEndpointAuthMethod != "r-method" {
		t.Fatalf("partial backfill: %+v", ocfg)
	}
}

func TestSnapshotClientReg(t *testing.T) {
	h := transport.NewOAuthHandler(transport.OAuthConfig{
		ClientID: "cid", ClientSecret: "csec",
		CredentialsIssuer: "https://iss", TokenEndpointAuthMethod: "client_secret_post",
	})
	got := SnapshotClientReg(h)
	if got.ClientID != "cid" || got.ClientSecret != "csec" ||
		got.Issuer != "https://iss" || got.AuthMethod != "client_secret_post" {
		t.Fatalf("snapshot: %+v", got)
	}
	zero := SnapshotClientReg(transport.NewOAuthHandler(transport.OAuthConfig{}))
	if zero != (ClientReg{}) {
		t.Fatalf("zero handler: %+v", zero)
	}
}

func TestPendingRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oauth")
	key := "srv"
	p := PendingFlow{
		State: "st", Verifier: "ver", RedirectURI: "http://localhost:1/callback",
		ClientID: "cid", ClientSecret: "sec", AuthMethod: "none", Issuer: "https://iss",
	}
	if err := SavePending(dir, key, p); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, key+".pending.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("pending mode: %v %v", st, err)
	}
	got, err := LoadPending(context.Background(), dir, key)
	if err != nil || got == nil || *got != p {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadPending(canceled, dir, key); err == nil {
		t.Fatal("canceled ctx must error")
	}
	ClearPending(dir, key)
	if got, err := LoadPending(context.Background(), dir, key); got != nil || err != nil {
		t.Fatalf("after Clear: %+v %v", got, err)
	}
	ClearPending(dir, key) // missing: no panic
	// Missing file, empty JSON {}, empty state, empty verifier, corrupt:
	// all return nil,nil (current behavior).
	if got, err := LoadPending(context.Background(), dir, "absent"); got != nil || err != nil {
		t.Fatalf("missing: %+v %v", got, err)
	}
	for name, content := range map[string]string{
		"empty-object": "{}", "corrupt": "{bad",
	} {
		if err := os.WriteFile(filepath.Join(dir, key+".pending.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := LoadPending(context.Background(), dir, key); got != nil || err != nil {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	for _, tc := range []PendingFlow{{Verifier: "v"}, {State: "s"}} {
		if err := SavePending(dir, key, tc); err != nil {
			t.Fatal(err)
		}
		if got, err := LoadPending(context.Background(), dir, key); got != nil || err != nil {
			t.Fatalf("partial %+v: %+v %v", tc, got, err)
		}
	}
	// SavePending with entirely empty fields writes a file that Load treats
	// as absent (documents the asymmetry).
	if err := SavePending(dir, key, PendingFlow{}); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadPending(context.Background(), dir, key); got != nil || err != nil {
		t.Fatalf("empty save: %+v %v", got, err)
	}
}
