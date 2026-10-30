package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestCallbackServerBasics(t *testing.T) {
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.Port() == 0 {
		t.Fatal("want non-zero ephemeral port")
	}
	want := fmt.Sprintf("http://localhost:%d/callback", cs.Port())
	if got := cs.RedirectURL(); got != want {
		t.Fatalf("RedirectURL: got %q want %q", got, want)
	}
	cs.Close()
	if _, err := http.Get(cs.RedirectURL()); err == nil {
		t.Fatal("want request to fail after Close")
	}
}

func TestCallbackServerFixedPortFallback(t *testing.T) {
	// Current behavior: an occupied fixed port silently falls back to an
	// ephemeral port instead of returning an error (oauth.go NewCallbackServer).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	cs, err := NewCallbackServer(occupied)
	if err != nil {
		t.Fatalf("occupied fixed port should fall back, got error: %v", err)
	}
	defer cs.Close()
	if cs.Port() == occupied {
		t.Fatal("fell back expected: bound the occupied port")
	}
}

func TestWaitSuccessAndSlotRelease(t *testing.T) {
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := cs.Wait(context.Background(), "s1")
		done <- result{code, err}
	}()
	time.Sleep(50 * time.Millisecond) // let Wait register before show
	resp, err := http.Get(cs.RedirectURL() + "?code=c1&state=s1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if string(body) == "" {
		t.Fatal("want non-empty success body")
	}
	select {
	case r := <-done:
		if r.err != nil || r.code != "c1" {
			t.Fatalf("Wait: code=%q err=%v", r.code, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return")
	}
	// Slot released: a second callback for the same state is unknown.
	resp2, err := http.Get(cs.RedirectURL() + "?code=c1&state=s1")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest || string(b2) != "unknown oauth state" {
		t.Fatalf("second callback: status=%d body=%q", resp2.StatusCode, b2)
	}
}

func TestCallbackErrorBranches(t *testing.T) {
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	// access_denied with description.
	w1 := cs.Prepare("e1")
	done := make(chan error, 1)
	go func() { _, err := w1.Await(context.Background()); done <- err }()
	time.Sleep(20 * time.Millisecond)
	resp, err := http.Get(cs.RedirectURL() + "?error=access_denied&error_description=nope&state=e1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("error callback status: %d (current behavior: 200 even on error)", resp.StatusCode)
	}
	select {
	case err := <-done:
		if err == nil || !containsAll(err.Error(), "access_denied", "nope") {
			t.Fatalf("Await error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await did not return")
	}

	// Missing code.
	w2 := cs.Prepare("e2")
	done2 := make(chan error, 1)
	go func() { _, err := w2.Await(context.Background()); done2 <- err }()
	time.Sleep(20 * time.Millisecond)
	resp2, err := http.Get(cs.RedirectURL() + "?state=e2")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("missing-code status: %d", resp2.StatusCode)
	}
	select {
	case err := <-done2:
		if err == nil || !containsAll(err.Error(), "missing code") {
			t.Fatalf("missing code error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await did not return")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestAwaitCancel(t *testing.T) {
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	w := cs.Prepare("cancel-me")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := w.Await(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await with 10ms ctx: %v", err)
	}
	// Slot released by Await: a later callback is unknown.
	resp, err := http.Get(cs.RedirectURL() + "?code=c&state=cancel-me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("late callback status: %d", resp.StatusCode)
	}
}

func TestPrepareCancelReleasesSlot(t *testing.T) {
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	cs.Prepare("slot").Cancel()
	resp, err := http.Get(cs.RedirectURL() + "?code=c&state=slot")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback after Cancel: %d", resp.StatusCode)
	}
}

func TestPrepareSameStateTwiceDocumented(t *testing.T) {
	// Current behavior: a second Prepare with the same state overwrites the
	// map entry, orphaning the first waiter; the first Await times out and its
	// deferred Cancel then deletes the second slot too. Documented; a fix
	// would reject duplicate states or close the old channel.
	cs, err := NewCallbackServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	w1 := cs.Prepare("dup")
	_ = cs.Prepare("dup")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := w1.Await(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Await: %v", err)
	}
	resp, err := http.Get(cs.RedirectURL() + "?code=c&state=dup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback after double-Prepare + first timeout: %d (slot clobbered)", resp.StatusCode)
	}
}
