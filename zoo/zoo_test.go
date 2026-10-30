// Zoo battery: point the oxyp daemon at every stdio breakage case and
// record survive/fail. Run: go test ./zoo/ -v. Requires python3 + node.
package zoo

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"oxyp/internal/config"
	"oxyp/internal/daemon"
)

func zooPath(t *testing.T, parts ...string) string {
	t.Helper()
	// repo root = parent of zoo/
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := wd
	if filepath.Base(wd) == "zoo" {
		root = filepath.Dir(wd)
	}
	return filepath.Join(append([]string{root, "zoo"}, parts...)...)
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// withoutSidecars drops oauth sidecar files (<key>.client.json,
// <key>.pending.json) from a "*.json" glob so token-file assertions count
// tokens only.
func withoutSidecars(matches []string) []string {
	out := matches[:0]
	for _, m := range matches {
		if strings.HasSuffix(m, ".client.json") || strings.HasSuffix(m, ".pending.json") {
			continue
		}
		out = append(out, m)
	}
	return out
}

type zooCase struct {
	name    string
	argv    func(base string) []string
	timeout time.Duration
	// wantErr: substring that must appear in error, "" = must succeed
	wantListErr string
	wantCallErr string
	callArgs    map[string]any
	callTool    string
}

func stdioCases(py string) []zooCase {
	z := func(args ...string) func(string) []string {
		return func(base string) []string {
			return append([]string{py, filepath.Join(base, "stdio", "zoo_stdio.py")}, args...)
		}
	}
	return []zooCase{
		{name: "clean", argv: z(), callTool: "echo_tool", callArgs: map[string]any{"msg": "x"}},
		{name: "junk-stdout", argv: z("--junk-stdout"), wantListErr: "", callTool: "echo_tool", callArgs: map[string]any{"msg": "x"}},
		{name: "blank-stdout", argv: z("--blank-stdout"), callTool: "echo_tool", callArgs: map[string]any{"msg": "x"}},
		{name: "stderr-flood", argv: z("--stderr-flood"), callTool: "ping_tool"},
		{name: "slow-init-2s", argv: z("--slow-init", "2"), callTool: "ping_tool"},
		{name: "slow-tool-2s", argv: z("--slow-tool", "2"), callTool: "ping_tool"},
		{name: "crash-on-3", argv: z("--crash-on", "3"), wantListErr: "transport closed"},
		{name: "bad-json-on-call", argv: z("--bad-json"), callTool: "ping_tool", wantCallErr: "deadline exceeded"},
		{name: "wrong-version", argv: z("--wrong-version"), wantListErr: "unsupported protocol version"},
		{name: "paginated-150", argv: z("--paginated", "150"), callTool: "tool_001"},
		{name: "unicode-names", argv: z("--unicode-names"), callTool: "ping_tool"},
		{name: "dup-names", argv: z("--dup-names"), callTool: "ping_tool"},
		{name: "zero-tools", argv: z("--zero-tools")},
		{name: "bad-schema", argv: z("--bad-schema"), wantListErr: "unmarshal"},
		{name: "giant-result-1mb", argv: z("--giant-result", "1"), callTool: "ping_tool"},
		{name: "notify-changed", argv: z("--notify-changed", "1"), callTool: "ping_tool"},
	}
}

func TestStdioZoo(t *testing.T) {
	python := "python3"
	for _, c := range stdioCases(python) {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{Servers: map[string]config.ServerEntry{
				"zoo": {Command: c.argv(zooPath(t))},
			}}
			d := daemon.New(cfg, testLog())
			defer d.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			tools, err := d.ListTools(ctx, "zoo")
			if c.wantListErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantListErr) {
					t.Fatalf("list: want err containing %q, got tools=%d err=%v", c.wantListErr, len(tools), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("list failed: %v", err)
			}
			t.Logf("listed %d tools", len(tools))
			if c.callTool == "" {
				return
			}
			res, err := d.CallTool(ctx, "zoo", c.callTool, c.callArgs)
			if c.wantCallErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantCallErr) {
					t.Fatalf("call: want err containing %q, got %v", c.wantCallErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("call %s failed: %v", c.callTool, err)
			}
			if res.IsError {
				t.Fatalf("call %s returned isError: %+v", c.callTool, res.Content)
			}
		})
	}
}

func TestResourceZoo(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"zoo": {Command: []string{"python3",
			filepath.Join(zooPath(t), "stdio", "zoo_stdio.py"), "--with-resource"}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Instructions surface over the daemon API (HTTP list wraps this).
	ins, err := d.Instructions(ctx, "zoo")
	if err != nil {
		t.Fatalf("instructions: %v", err)
	}
	if !strings.Contains(ins, "zoo says hi") {
		t.Fatalf("instructions = %q", ins)
	}
	res, err := d.ListResources(ctx, "zoo")
	if err != nil || len(res) != 1 || res[0].URI != "zoo://greeting" {
		t.Fatalf("list resources: %+v err=%v", res, err)
	}
	body, err := d.ReadResource(ctx, "zoo", "zoo://greeting")
	if err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if len(body.Contents) != 1 {
		t.Fatalf("contents = %+v", body.Contents)
	}
}

func TestResourceUnsupportedIsEmpty(t *testing.T) {
	// Clean zoo has no resources support: mcp-go returns -32601.
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"zoo": {Command: []string{"python3",
			filepath.Join(zooPath(t), "stdio", "zoo_stdio.py")}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.ListResources(ctx, "zoo"); err == nil {
		t.Fatal("clean zoo should fail resources/list (meta maps it to [])")
	}
}

func TestSpawnEnvCwdZoo(t *testing.T) {
	// The zoo's env_echo tool reports its own env + cwd: proves
	// inherit_env/cwd reach the spawned child.
	t.Setenv("OXYP_ZOO_SENTINEL", "from-daemon")
	// Resolve symlinks: the child reports getcwd (physical path).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := false
	mkcfg := func(extra config.ServerEntry) *config.Config {
		extra.Command = []string{"python3",
			filepath.Join(zooPath(t), "stdio", "zoo_stdio.py"), "--env-echo"}
		return &config.Config{Servers: map[string]config.ServerEntry{"zoo": extra}}
	}
	callText := func(d *daemon.Daemon) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.ListTools(ctx, "zoo"); err != nil {
			t.Fatalf("list: %v", err)
		}
		res, err := d.CallTool(ctx, "zoo", "env_echo", nil)
		if err != nil || res.IsError {
			t.Fatalf("call: %v %+v", err, res)
		}
		for _, b := range res.Content {
			if tc, ok := b.(mcp.TextContent); ok {
				return tc.Text
			}
		}
		t.Fatal("no text block in env_echo result")
		return ""
	}
	d1 := daemon.New(mkcfg(config.ServerEntry{}), testLog())
	defer d1.Close()
	got := callText(d1)
	if !strings.Contains(got, `"OXYP_ZOO_SENTINEL": "from-daemon"`) {
		t.Fatalf("default child must inherit daemon env: %s", got)
	}
	d2 := daemon.New(mkcfg(config.ServerEntry{InheritEnv: &f, Cwd: dir,
		Env: map[string]string{"EXPLICIT": "yes"}}), testLog())
	defer d2.Close()
	got2 := callText(d2)
	if strings.Contains(got2, `"OXYP_ZOO_SENTINEL": "from-daemon"`) {
		t.Fatalf("clean child must not inherit daemon env: %s", got2)
	}
	if !strings.Contains(got2, `"EXPLICIT": "yes"`) {
		t.Fatalf("explicit overlay missing: %s", got2)
	}
	if !strings.Contains(got2, dir) {
		t.Fatalf("cwd not honored: %s", got2)
	}
}

func TestNodeZoo(t *testing.T) {
	node := "node"
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"node": {Command: []string{node, filepath.Join(zooPath(t), "stdio", "zoo_node.js")}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := d.ListTools(ctx, "node")
	if err != nil || len(tools) != 2 {
		t.Fatalf("node list: tools=%d err=%v", len(tools), err)
	}
	res, err := d.CallTool(ctx, "node", "node_echo", map[string]any{"msg": "hi"})
	if err != nil || res.IsError {
		t.Fatalf("node call: %v %+v", err, res)
	}
}

func TestNeedyZoo(t *testing.T) {
	cfg := &config.Config{Servers: map[string]config.ServerEntry{
		"needy": {Command: []string{"python3", filepath.Join(zooPath(t), "stdio", "zoo_needy.py")}},
	}}
	d := daemon.New(cfg, testLog())
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	tools, err := d.ListTools(ctx, "needy")
	if err != nil || len(tools) != 1 {
		t.Fatalf("needy list: tools=%d err=%v", len(tools), err)
	}
	// sampling/elicitation/roots requests from server must not hang us:
	// mcp-go client should decline; call completes or fails fast (not hang).
	res, err := d.CallTool(ctx, "needy", "needy_tool", nil)
	t.Logf("needy call: res=%+v err=%v", res, err)
}
