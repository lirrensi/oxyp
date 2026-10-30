package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/daemon"
	"github.com/lirrensi/oxyp/internal/importcfg"
)

// doctorOAuthDir keeps doctor.go's daemon construction in one place.
func doctorOAuthDir(dir string) string { return filepath.Join(dir, "oauth") }

// confirmWrite prompts "Write to <path>? [y/N]" on stderr and reports the
// answer. EOF / non-terminal stdin counts as no.
func confirmWrite(path string) bool {
	fmt.Fprintf(os.Stderr, "Write to %s? [y/N] ", path)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// loadOrEmpty loads the config, tolerating a missing oxyp.toml (add/import
// create it). Parse errors still fail: never append to a broken file blind.
func loadOrEmpty(dir string) (*config.Config, error) {
	if _, err := os.Stat(importcfg.ConfigPath(dir)); err != nil {
		if os.IsNotExist(err) {
			return &config.Config{Servers: map[string]config.ServerEntry{}}, nil
		}
		return nil, err
	}
	return config.Load(dir)
}

func takenNames(cfg *config.Config) map[string]bool {
	taken := make(map[string]bool, len(cfg.Servers))
	for n := range cfg.Servers {
		taken[n] = true
	}
	return taken
}

func addCmd() *cobra.Command {
	var nameFlag string
	var writeFlag bool
	c := &cobra.Command{
		Use:   "add <url> [--name]",
		Short: "validate + probe a remote URL and append a [servers.<name>] entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			dir := oxypDir()
			rawurl := strings.TrimSpace(args[0])

			base := strings.TrimSpace(nameFlag)
			if base == "" {
				derived, err := importcfg.NameFromURL(rawurl)
				if err != nil {
					return err
				}
				base = derived
			} else {
				base = importcfg.SanitizeName(base)
			}

			cfg, err := loadOrEmpty(dir)
			if err != nil {
				return err
			}
			name := importcfg.UniqueName(base, takenNames(cfg))

			entry := config.ServerEntry{
				Description: "Added via oxyp add",
				URL:         rawurl,
			}

			// Probe before writing: real connection, 15s budget.
			probeCfg := &config.Config{Servers: map[string]config.ServerEntry{name: entry}}
			d := daemon.NewWithOAuthDir(probeCfg, nil, filepath.Join(dir, "oauth"))
			needsAuth := false
			toolCount := 0
			func() {
				defer d.Close()
				ctx, cancel := context.WithTimeout(context.Background(), liveBudget)
				defer cancel()
				tools, perr := d.ListTools(ctx, name)
				if perr != nil {
					var authErr *daemon.AuthNeededError
					if errors.As(perr, &authErr) {
						needsAuth = true
						return
					}
					err = fmt.Errorf("probe %q failed: %w", rawurl, perr)
					return
				}
				toolCount = len(tools)
			}()
			if err != nil {
				return err
			}

			var notes []string
			if needsAuth {
				notes = append(notes, `server needs auth, verify with: oxyp login `+name)
			}
			block, err := importcfg.RenderBlock(name, entry, notes)
			if err != nil {
				return err
			}
			if needsAuth {
				fmt.Printf("%s: needs-auth (reachable, login required)\n\n", name)
			} else {
				fmt.Printf("%s: ok (%d tools)\n\n", name, toolCount)
			}
			fmt.Print(block)

			path := importcfg.ConfigPath(dir)
			if !writeFlag && !confirmWrite(path) {
				fmt.Fprintln(os.Stderr, "not written")
				return nil
			}
			note, err := importcfg.AppendBlocks(dir, block)
			if err != nil {
				return err
			}
			if note != "" {
				fmt.Fprintln(os.Stderr, note)
			}
			fmt.Fprintf(os.Stderr, "added [servers.%s] to %s\n", name, path)
			if needsAuth {
				fmt.Fprintf(os.Stderr, "next: oxyp login %s\n", name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&nameFlag, "name", "", "server name (default: derived from URL hostname)")
	c.Flags().BoolVar(&writeFlag, "write", false, "write without prompting")
	return c
}
