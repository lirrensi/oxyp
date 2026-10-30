package zoo

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
)

// Search runs on cached metadata only: warm the disk cache via one live
// list, restart the daemon cold, search must answer without spawning.
func TestSearchColdFromDisk(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	entry := config.ServerEntry{
		Command:     []string{"python3", zooPath(t, "stdio", "zoo_stdio.py"), "--paginated", "150"},
		Description: "many tools here",
	}
	cfg := &config.Config{Servers: map[string]config.ServerEntry{"zoo": entry}}
	d := daemon.NewWithDirs(cfg, testLog(), "", cachePath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.ListTools(ctx, "zoo"); err != nil {
		t.Fatalf("warm list: %v", err)
	}
	d.Close()

	// cold daemon, same entry: search must answer from disk without spawn
	cfg3 := &config.Config{Servers: map[string]config.ServerEntry{"zoo": entry}}
	d3 := daemon.NewWithDirs(cfg3, testLog(), "", cachePath)
	defer d3.Close()

	hits, total, next, err := d3.SearchTools("", "tool_001", false, 12, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 1 || len(hits) != 1 || hits[0].Name != "tool_001" {
		t.Fatalf("search tool_001: total=%d hits=%+v next=%d", total, hits, next)
	}
	// broad query hits many → paging works
	hits, total, next, err = d3.SearchTools("", "tool_", false, 10, 0)
	if err != nil || total < 100 || len(hits) != 10 || next != 10 {
		t.Fatalf("paged search: total=%d len=%d next=%d err=%v", total, len(hits), next, err)
	}
	// regex mode
	hits, total, _, err = d3.SearchTools("", "zoo/tool_00[12]", true, 12, 0)
	if err != nil || total != 2 {
		t.Fatalf("regex search: total=%d hits=%+v err=%v", total, hits, err)
	}
	// cold daemon never spawned
	d3s := d3.Entry("zoo")
	if d3s == nil {
		t.Fatal("entry missing")
	}
	// verify via health: no health record means never connected
	if h := d3.HealthOf("zoo"); h != nil {
		t.Fatalf("search must not connect: health=%+v", h)
	}
}

func TestSearchRespectsDeny(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"zoo": {
			Command: []string{"python3", zooPath(t, "stdio", "zoo_stdio.py")},
			Tools:   &config.ToolsPolicy{Deny: []string{"echo_*"}},
		},
	}}
	d := daemon.NewWithDirs(cfg, testLog(), "", cachePath)
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.ListTools(ctx, "zoo"); err != nil {
		t.Fatalf("warm list: %v", err)
	}
	hits, total, _, err := d.SearchTools("", "echo", false, 12, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 0 || len(hits) != 0 {
		t.Fatalf("denied tools must be invisible to search: %+v", hits)
	}
	hits, total, _, err = d.SearchTools("", "ping", false, 12, 0)
	if err != nil || total != 1 || hits[0].Name != "ping_tool" {
		t.Fatalf("allowed tool missing: total=%d hits=%+v", total, hits)
	}
}
