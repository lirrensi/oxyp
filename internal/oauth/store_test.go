package oauth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestStorageKey(t *testing.T) {
	k := StorageKey("name", "https://example.com/mcp")
	if len(k) != 32 {
		t.Fatalf("want 32 chars, got %q", k)
	}
	for _, c := range k {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("want lowercase hex, got %q", k)
		}
	}
	if _, err := hex.DecodeString(k); err != nil {
		t.Fatalf("not hex: %v", err)
	}
	if got := StorageKey("name", "https://example.com/mcp"); got != k {
		t.Fatal("not deterministic")
	}
	if a, b := StorageKey("nameA", "urlB"), StorageKey("urlB", "nameA"); a == b {
		t.Fatal("name/URL swap must differ")
	}
	// NUL separator keeps ("ab","c") distinct from ("a","bc").
	if a, b := StorageKey("ab", "c"), StorageKey("a", "bc"); a == b {
		t.Fatal("separator collision")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oauth")
	s, err := NewStore(dir, "key1")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", st, err)
	}
	ctx := context.Background()
	tok := &transport.Token{
		Issuer: "https://iss", AccessToken: "acc", TokenType: "Bearer",
		RefreshToken: "ref", ExpiresIn: 3600, Scope: "s",
		ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := s.SaveToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, "key1.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("tmp leftovers: %v", leftovers)
	}
	got, err := s.GetToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Issuer != tok.Issuer || got.AccessToken != tok.AccessToken ||
		got.TokenType != tok.TokenType || got.RefreshToken != tok.RefreshToken ||
		got.ExpiresIn != tok.ExpiresIn || got.Scope != tok.Scope ||
		!got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !s.HasRefreshToken(ctx) {
		t.Fatal("want HasRefreshToken true")
	}
	// Overwrite drops the refresh token.
	if err := s.SaveToken(ctx, &transport.Token{AccessToken: "acc2", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetToken(ctx)
	if err != nil || got.AccessToken != "acc2" {
		t.Fatalf("overwrite: %+v %v", got, err)
	}
	if s.HasRefreshToken(ctx) {
		t.Fatal("want HasRefreshToken false after overwrite")
	}
	s.Delete()
	if _, err := s.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("after Delete: %v", err)
	}
	if s.HasRefreshToken(ctx) {
		t.Fatal("want HasRefreshToken false after Delete")
	}
	s.Delete() // missing file: no panic
}

func TestStoreMissingCorruptCanceled(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "missing")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "missing.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("corrupt: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetToken(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Get: %v", err)
	}
	s2, err := NewStore(dir, "canceled-save")
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.SaveToken(canceled, &transport.Token{AccessToken: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "canceled-save.json")); !os.IsNotExist(err) {
		t.Fatal("canceled Save must write nothing")
	}
	if s2.HasRefreshToken(canceled) {
		t.Fatal("canceled HasRefreshToken must be false")
	}
}

func TestStoreConcurrent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "shared")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			_ = s.SaveToken(ctx, &transport.Token{AccessToken: string(rune('a' + i))})
			_, _ = s.GetToken(ctx)
			_ = s.HasRefreshToken(ctx)
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(dir, "shared.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tok transport.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		t.Fatalf("torn write: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("empty token after concurrent saves")
	}
}

func TestStoreTwoInstancesTempCollision(t *testing.T) {
	// Current behavior: both Stores share one fixed "<key>.json.tmp"
	// (oauth.go SaveToken). Small writes are single syscalls so the
	// renamed file is always one complete token; recommend os.CreateTemp.
	dir := t.TempDir()
	a, err := NewStore(dir, "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStore(dir, "same")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = a.SaveToken(context.Background(), &transport.Token{AccessToken: "writer-one", TokenType: "Bearer"})
		}()
		go func() {
			defer wg.Done()
			_ = b.SaveToken(context.Background(), &transport.Token{AccessToken: "writer-two", TokenType: "Bearer"})
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(filepath.Join(dir, "same.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tok transport.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("torn cross-instance write: %v", err)
	}
	if tok.AccessToken != "writer-one" && tok.AccessToken != "writer-two" {
		t.Fatalf("unexpected token: %q", tok.AccessToken)
	}
}
