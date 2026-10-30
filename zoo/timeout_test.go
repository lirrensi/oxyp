package zoo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
)

// Slow timeout edges: prove timeouts fire and report cleanly.
// Slow by nature (~50s total); run separately: go test ./zoo/ -run Timeout -v.
func TestTimeoutEdges(t *testing.T) {
	py := "python3"
	zoo := filepath.Join(zooPath(t), "stdio", "zoo_stdio.py")

	t.Run("slow-tool-15s-exceeds-10s-request-timeout", func(t *testing.T) {
		cfg := &config.Config{Servers: map[string]config.ServerEntry{
			"slow": {Command: []string{py, zoo, "--slow-tool", "15"}},
		}}
		d := daemon.New(cfg, testLog())
		defer d.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.ListTools(ctx, "slow"); err != nil {
			t.Fatalf("list: %v", err)
		}
		start := time.Now()
		_, err := d.CallTool(ctx, "slow", "ping_tool", nil)
		elapsed := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("want deadline exceeded, got %v", err)
		}
		if elapsed > 13*time.Second {
			t.Fatalf("timeout fired too late: %v", elapsed)
		}
		t.Logf("timed out cleanly after %v", elapsed)
	})

	t.Run("hang-tool", func(t *testing.T) {
		cfg := &config.Config{Servers: map[string]config.ServerEntry{
			"hang": {Command: []string{py, zoo, "--hang-tool"}},
		}}
		d := daemon.New(cfg, testLog())
		defer d.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.ListTools(ctx, "hang"); err != nil {
			t.Fatalf("list: %v", err)
		}
		start := time.Now()
		_, err := d.CallTool(ctx, "hang", "ping_tool", nil)
		elapsed := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("want deadline exceeded, got %v", err)
		}
		if elapsed > 13*time.Second {
			t.Fatalf("timeout fired too late: %v", elapsed)
		}
		t.Logf("hang killed cleanly after %v", elapsed)
	})
}
