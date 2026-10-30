package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lirrensi/oxyp/internal/importcfg"
)

func importCmd() *cobra.Command {
	var fromFlag string
	var writeFlag bool
	c := &cobra.Command{
		Use:   "import [--from cursor|claude|vscode|opencode|omp|all] [--write]",
		Short: "preview third-party MCP configs as oxyp TOML, write on confirmation",
		RunE: func(_ *cobra.Command, _ []string) error {
			from := strings.ToLower(strings.TrimSpace(fromFlag))
			if from == "" {
				from = "all"
			}
			switch from {
			case "all", "cursor", "claude", "vscode", "opencode", "omp":
			default:
				return fmt.Errorf("unknown --from %q (want cursor|claude|vscode|opencode|omp|all)", from)
			}

			dir := oxypDir()
			cfg, err := loadOrEmpty(dir)
			if err != nil {
				return err
			}
			taken := takenNames(cfg)

			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("no home dir: %w", err)
			}
			var blocks []importcfg.NamedBlock
			for _, src := range importcfg.Sources(home) {
				if from != "all" && src.Name != from {
					continue
				}
				path := importcfg.FindFile(src)
				if path == "" {
					fmt.Fprintf(os.Stderr, "%s: no config found, skipped\n", src.Name)
					continue
				}
				data, err := os.ReadFile(path)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: cannot read %s: %v\n", src.Name, path, err)
					continue
				}
				raw, err := importcfg.Parse(src.Name, data)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: cannot parse %s: %v\n", src.Name, path, err)
					continue
				}
				names := make([]string, 0, len(raw))
				for n := range raw {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					r := raw[n]
					entry, notes, converr := importcfg.ToOxyp(src.Name, r)
					if converr != nil {
						fmt.Fprintf(os.Stderr, "%s/%s: dropped: %v\n", src.Name, n, converr)
						continue
					}
					final := importcfg.UniqueName(importcfg.SanitizeName(n), taken)
					taken[final] = true
					text, err := importcfg.RenderBlock(final, entry, notes)
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s/%s: cannot render: %v\n", src.Name, n, err)
						continue
					}
					blocks = append(blocks, importcfg.NamedBlock{Name: final, Text: text})
				}
			}

			if len(blocks) == 0 {
				fmt.Fprintln(os.Stderr, "nothing to import")
				return nil
			}
			// ALWAYS preview the exact TOML first.
			text := importcfg.RenderAll(blocks)
			fmt.Print(text)

			path := importcfg.ConfigPath(dir)
			if !writeFlag && !confirmWrite(path) {
				fmt.Fprintln(os.Stderr, "not written")
				return nil
			}
			note, err := importcfg.AppendBlocks(dir, text)
			if err != nil {
				return err
			}
			if note != "" {
				fmt.Fprintln(os.Stderr, note)
			}
			fmt.Fprintf(os.Stderr, "imported %d server(s) into %s\n", len(blocks), path)
			return nil
		},
	}
	c.Flags().StringVar(&fromFlag, "from", "all", "source app: cursor|claude|vscode|opencode|omp|all")
	c.Flags().BoolVar(&writeFlag, "write", false, "write without prompting")
	return c
}
