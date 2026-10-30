package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
)

// liveBudget is the per-server probe budget for doctor --live (adapter parity).
const liveBudget = 15 * time.Second

type doctorResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok|failed|needs-auth|disabled
	Tools  int    `json:"tools,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func doctorCmd() *cobra.Command {
	var live, asJSON bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "validate config (offline) or probe every server (--live)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := requireLocal("doctor"); err != nil {
				return err
			}
			dir := oxypDir()
			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			if live {
				return runDoctorLive(dir, cfg, asJSON)
			}
			return runDoctorOffline(cfg, asJSON)
		},
	}
	c.Flags().BoolVar(&live, "live", false, "connect every configured server (15s budget each)")
	c.Flags().BoolVar(&asJSON, "json", false, "print results as a JSON array")
	return c
}

func sortedNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// runDoctorOffline parses config + per-entry validation only. Never touches
// the network: ConfigError/disabled come straight from config.Load.
func runDoctorOffline(cfg *config.Config, asJSON bool) error {
	results := make([]doctorResult, 0, len(cfg.Servers))
	for _, name := range sortedNames(cfg) {
		e := cfg.Servers[name]
		switch {
		case !e.IsEnabled():
			results = append(results, doctorResult{Name: name, Status: "disabled", Detail: "skipped (enabled = false)"})
		case e.ConfigError != "":
			results = append(results, doctorResult{Name: name, Status: "failed", Detail: "config: " + e.ConfigError})
		default:
			results = append(results, doctorResult{Name: name, Status: "ok", Detail: doctorKind(e)})
		}
	}
	printDoctor(results, false, asJSON)
	return doctorExit(results)
}

func doctorKind(e config.ServerEntry) string {
	if e.IsRemote() {
		return "remote " + e.URL
	}
	return fmt.Sprintf("local %q", e.Command)
}

// runDoctorLive connects EVERY configured server independently with a 15s
// budget each. One broken server never breaks the others or the command:
// failures are recorded per-server and the loop continues.
func runDoctorLive(dir string, cfg *config.Config, asJSON bool) error {
	names := sortedNames(cfg)
	results := make([]doctorResult, 0, len(names))

	// In-process daemon with no disk cache: every ListTools below is a real
	// connection, never a cold-cache hit. Disabled servers are excluded by
	// New, so they are reported without probing.
	d := daemon.NewWithOAuthDir(cfg, nil, doctorOAuthDir(dir))
	defer d.Close()

	for _, name := range names {
		e := cfg.Servers[name]
		switch {
		case !e.IsEnabled():
			results = append(results, doctorResult{Name: name, Status: "disabled", Detail: "skipped (enabled = false)"})
			continue
		case e.ConfigError != "":
			results = append(results, doctorResult{Name: name, Status: "failed", Detail: "config: " + e.ConfigError})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), liveBudget)
		tools, err := d.ListTools(ctx, name)
		cancel()
		if err != nil {
			var authErr *daemon.AuthNeededError
			if errors.As(err, &authErr) {
				results = append(results, doctorResult{
					Name: name, Status: "needs-auth",
					Detail: "run: oxyp login " + name,
				})
				continue
			}
			results = append(results, doctorResult{Name: name, Status: "failed", Detail: err.Error()})
			continue
		}
		results = append(results, doctorResult{Name: name, Status: "ok", Tools: len(tools), Detail: doctorKind(e)})
	}
	printDoctor(results, true, asJSON)
	return doctorExit(results)
}

func printDoctor(results []doctorResult, live, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return
	}
	for _, r := range results {
		fmt.Println(doctorLine(r, live))
	}
}

func doctorLine(r doctorResult, live bool) string {
	switch r.Status {
	case "ok":
		if live {
			return fmt.Sprintf("%s: ok (%d tools, %s)", r.Name, r.Tools, r.Detail)
		}
		return fmt.Sprintf("%s: ok (%s)", r.Name, r.Detail)
	case "disabled":
		return fmt.Sprintf("%s: disabled (%s)", r.Name, r.Detail)
	case "needs-auth":
		return fmt.Sprintf("%s: needs-auth (%s)", r.Name, r.Detail)
	default:
		return fmt.Sprintf("%s: failed (%s)", r.Name, r.Detail)
	}
}

// doctorExit returns nil only when every enabled server is ok. Disabled
// servers are ignored; failed and needs-auth both exit 1.
func doctorExit(results []doctorResult) error {
	bad := 0
	for _, r := range results {
		if r.Status == "failed" || r.Status == "needs-auth" {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("doctor: %d server(s) need attention", bad)
	}
	return nil
}
