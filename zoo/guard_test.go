package zoo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oxyp/internal/config"
	"oxyp/internal/daemon"
	"oxyp/internal/meta"
)

// Armed guard caps giant text; disarmed guard passes it whole.
func TestOutputGuardArmed(t *testing.T) {
	giant := func(t *testing.T, guard *config.OutputGuardConfig) map[string]any {
		t.Helper()
		cfg := &config.Config{Servers: map[string]config.ServerEntry{
			"zoo": {
				Command:     []string{"python3", zooPath(t, "stdio", "zoo_stdio.py"), "--giant-result", "1"},
				OutputGuard: guard,
			},
		}}
		d := daemon.New(cfg, testLog())
		defer d.Close()
		srv := meta.New(d, func() (a, r, m []string, err error) { return nil, nil, nil, nil }, "", "127.0.0.1:8379")
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/servers/zoo/tools/ping_tool", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	// off: full 1MB inline
	off := giant(t, nil)
	text := off["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(text) != 1024*1024 {
		t.Fatalf("guard off must pass whole: len=%d", len(text))
	}

	// armed: capped + spill note
	on := giant(t, &config.OutputGuardConfig{MaxBytes: 1000, MaxLines: 2000})
	text = on["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(text) > 2000 || !strings.Contains(text, "output guard") || !strings.Contains(text, "oxyp-output-") {
		t.Fatalf("guard on must cap+note: len=%d head=%.80q", len(text), text)
	}
}
