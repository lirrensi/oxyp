package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/lirrensi/oxyp/internal/config"
	"github.com/lirrensi/oxyp/internal/importcfg"
	peerfederation "github.com/lirrensi/oxyp/internal/peerfederation"
)

func peersCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "peers",
		Short: "manage federated peer daemons (add/list/remove/sync)",
	}
	c.AddCommand(peersAddCmd(), peersListCmd(), peersRemoveCmd(), peersSyncCmd())
	return c
}

func peersAddCmd() *cobra.Command {
	var asFlag, tokenFlag, modeFlag, descFlag string
	var writeFlag, forceFlag bool
	var priorityFlag int
	c := &cobra.Command{
		Use:   "add <url> [--as name] [--token] [--description]",
		Short: "handshake a peer daemon and append a [peers.<name>] entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := requireLocal("peers add"); err != nil {
				return err
			}
			dir := oxypDir()
			rawurl := strings.TrimSpace(args[0])
			mode := strings.ToLower(strings.TrimSpace(modeFlag))
			if mode == "" {
				mode = config.PeerModeMerge
			}
			if mode != config.PeerModeMerge && mode != config.PeerModeNamespace {
				return fmt.Errorf("mode must be merge|namespace")
			}

			// Handshake first: /peer reveals identity + shared preview.
			// Token: flag wins, else OXYP_PEER_TOKEN (never prompt: non-interactive safe).
			token := tokenFlag
			if token == "" {
				token = os.Getenv("OXYP_PEER_TOKEN")
			}
			pc, err := peerfederation.NewClient(rawurl, token, nil, 15*time.Second)
			if err != nil {
				return err
			}
			selfID, iderr := peerfederation.LoadOrCreateIdentity(dir)
			if iderr != nil {
				return fmt.Errorf("identity: %w", iderr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ctx = peerfederation.WithInstance(ctx, selfID)
			info, err := pc.Peer(ctx)
			if err != nil {
				var pe *peerfederation.PeerError
				if errors.As(err, &pe) && pe.Status == 401 {
					return fmt.Errorf("peer needs a token: re-run with --token (or OXYP_PEER_TOKEN)")
				}
				if errors.As(err, &pe) && pe.Status == http.StatusLoopDetected {
					return fmt.Errorf("that's you (loop refused): refusing self-mount")
				}
				return fmt.Errorf("handshake %q failed: %w", rawurl, err)
			}
			if info.Instance == "" {
				return fmt.Errorf("peer %q did not report an instance id (too old?)", rawurl)
			}
			if selfID != "" && info.Instance == selfID {
				return fmt.Errorf("that's you (instance %s): refusing self-mount", peerfederation.ShortID(selfID))
			}

			cfg, err := loadOrEmpty(dir)
			if err != nil {
				return err
			}
			// Duplicate URL? Refuse outright (exact match, no network).
			// --force overrides (same escape hatch as instance dups).
			if !forceFlag {
				for name, p := range cfg.Peers {
					if !p.IsEnabled() || p.ConfigError != "" {
						continue
					}
					if samePeerURL(p.URL, rawurl) {
						return fmt.Errorf("peer %q already mounts %s: remove it first (oxyp peers remove %s)", name, rawurl, name)
					}
				}
			}
			// Duplicate instance under another URL? Handshake each live
			// peer in parallel (bounded) and compare instance ids — direct
			// peers only, what we shook hands with. --force overrides
			// (migrations, split-brain recovery); sync-time collapse still
			// keeps one winner live.
			if !forceFlag {
				if dup := findDuplicateInstance(cfg, info.Instance, selfID); dup != "" {
					return fmt.Errorf("that daemon is already mounted as %q (instance %s): remove it first or re-run with --force", dup, peerfederation.ShortID(info.Instance))
				}
			}

			base := strings.TrimSpace(asFlag)
			if base == "" {
				base = info.Name
			}
			if base == "" {
				base = "peer"
			}
			base = importcfg.SanitizeName(base)
			if base == "" || base == "local" {
				base = "peer"
			}
			taken := map[string]bool{}
			for n := range cfg.Peers {
				taken[n] = true
			}
			for n := range cfg.Servers {
				taken[n] = true // bare claims collide with locals; keep unique
			}
			name := importcfg.UniqueName(base, taken)

			var b strings.Builder
			fmt.Fprintf(&b, "[peers.%s]\n", tomlKeyFor(name))
			if desc := strings.TrimSpace(descFlag); desc != "" {
				fmt.Fprintf(&b, "description = %q\n", desc)
			}
			fmt.Fprintf(&b, "url = %q\n", rawurl)
			if mode != config.PeerModeMerge {
				fmt.Fprintf(&b, "mode = %q\n", mode)
			}
			if priorityFlag != 0 {
				fmt.Fprintf(&b, "priority = %d\n", priorityFlag)
			}
			if token != "" {
				fmt.Fprintf(&b, "token = %q\n", token)
			}
			block := b.String()

			idShort := peerfederation.ShortID(info.Instance)
			dispName := info.Name
			if dispName == "" {
				dispName = "(anonymous)"
			}
			fmt.Printf("%s: ok (%s, instance %s, %d shared servers)\n\n", name, dispName, idShort, info.Count)
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
			fmt.Fprintf(os.Stderr, "added [peers.%s] to %s\n", name, path)
			if strings.TrimSpace(descFlag) == "" {
				fmt.Fprintln(os.Stderr, "tip: re-run with --description \"wsl box - holds linear token\" or edit oxyp.toml (description shows in peers list)")
			}
			// Best-effort live mount: reload a running daemon so the peer
			// appears without restart. Daemon down → it mounts at next start.
			if _, _, rerr := apiCall("POST", "/reload", nil); rerr == nil {
				fmt.Fprintln(os.Stderr, "reloaded: peer mounts now")
			}
			return nil
		},
	}
	c.Flags().StringVar(&asFlag, "as", "", "peer name (default: their daemon.name, else peer)")
	c.Flags().StringVar(&tokenFlag, "token", "", "peer bearer token (else OXYP_PEER_TOKEN)")
	c.Flags().StringVar(&modeFlag, "mode", "", "merge (default) or namespace")
	c.Flags().StringVar(&descFlag, "description", "", "what is this peer for, whose tokens? (shows in peers list, so nobody forgets)")
	c.Flags().IntVar(&priorityFlag, "priority", 0, "merge rank (lower wins; local = 0)")
	c.Flags().BoolVar(&writeFlag, "write", false, "write without prompting")
	c.Flags().BoolVar(&forceFlag, "force", false, "allow mounting a daemon already mounted under another name/URL")
	return c
}

// findDuplicateInstance handshakes every enabled configured peer in
// parallel (5s each, best-effort) and returns the name of one reporting
// wantInstance. Unreachable/misconfigured peers are skipped, never fatal:
// a dead peer can't confirm a dup, and sync-time collapse covers races.
func findDuplicateInstance(cfg *config.Config, wantInstance, selfID string) string {
	type hit struct {
		name     string
		instance string
	}
	ch := make(chan hit, len(cfg.Peers))
	var wg sync.WaitGroup
	for name, p := range cfg.Peers {
		if !p.IsEnabled() || p.ConfigError != "" || p.URL == "" {
			continue
		}
		wg.Add(1)
		go func(name string, p config.PeerEntry) {
			defer wg.Done()
			pc, err := peerfederation.NewClient(p.URL, p.Token, p.TLS, 5*time.Second)
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = peerfederation.WithInstance(ctx, selfID)
			h, err := pc.Health(ctx)
			if err != nil || h == nil {
				return
			}
			ch <- hit{name, h.Instance}
		}(name, p)
	}
	go func() { wg.Wait(); close(ch) }()
	for h := range ch {
		if h.instance != "" && h.instance == wantInstance {
			return h.name
		}
	}
	return ""
}

func peersListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list configured peers (static config view)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := requireLocal("peers list"); err != nil {
				return err
			}
			cfg, err := loadOrEmpty(oxypDir())
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Peers))
			for n := range cfg.Peers {
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) == 0 {
				fmt.Println("(no peers)")
				return nil
			}
			for _, n := range names {
				p := cfg.Peers[n]
				state := p.ModeOr()
				if !p.IsEnabled() {
					state = "disabled"
				} else if p.ConfigError != "" {
					state = "config: " + p.ConfigError
				}
				if desc := strings.TrimSpace(p.Description); desc != "" {
					fmt.Printf("%s: %s (%s, priority %d) — %s\n", n, p.URL, state, p.Priority, desc)
				} else {
					fmt.Printf("%s: %s (%s, priority %d)\n", n, p.URL, state, p.Priority)
				}
			}
			return nil
		},
	}
}

func peersRemoveCmd() *cobra.Command {
	var writeFlag bool
	c := &cobra.Command{
		Use:   "remove <name>",
		Short: "remove a [peers.<name>] entry (TOML rewrite)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := requireLocal("peers remove"); err != nil {
				return err
			}
			dir := oxypDir()
			name := args[0]
			cfg, err := loadOrEmpty(dir)
			if err != nil {
				return err
			}
			if _, ok := cfg.Peers[name]; !ok {
				return fmt.Errorf("peer %q not found", name)
			}
			path := importcfg.ConfigPath(dir)
			if !writeFlag && !confirmWrite(path) {
				fmt.Fprintln(os.Stderr, "not written")
				return nil
			}
			if err := removePeerBlock(dir, name); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "removed [peers.%s] from %s\n", name, path)
			if _, _, rerr := apiCall("POST", "/reload", nil); rerr == nil {
				fmt.Fprintln(os.Stderr, "reloaded: peer unmounts now")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&writeFlag, "write", false, "write without prompting")
	return c
}

// removePeerBlock drops the [peers.<name>] table (and its subtables) from
// oxyp.toml, preserving everything else byte-for-byte. Table-aware line scan:
// a [peers.<name>] header (bare or quoted key) starts the cut; the next
// top-level [table] header ends it.
func removePeerBlock(dir, name string) error {
	path := importcfg.ConfigPath(dir)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	out := make([]string, 0, len(lines))
	cutting, matched := false, false
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if strings.HasPrefix(trim, "[") {
			cutting = isPeerHeader(trim, name)
			if cutting {
				matched = true
			}
		}
		if !cutting {
			out = append(out, ln)
		}
	}
	if !matched {
		return fmt.Errorf("peer %q: no [peers.%s] table found in %s", name, name, path)
	}
	// Byte-precise: only the matched line range is gone; everything else
	// (blank runs, comments, leading lines) is untouched, mode preserved.
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), st.Mode().Perm())
}

// isPeerHeader reports whether a TOML [header] opens [peers.<name>] or one of
// its subtables ([peers.<name>.servers.x]). Bare and quoted keys both match.
func isPeerHeader(header, name string) bool {
	if !strings.HasPrefix(header, "[") {
		return false
	}
	// Strip a trailing TOML comment before parsing ("[peers.wsl] # box").
	// '#' inside quotes is a literal, not a comment — scan quote-aware.
	end := len(header)
	var quote byte
	for i := 0; i < len(header); i++ {
		c := header[i]
		if quote != 0 {
			if c == quote && header[i-1] != '\\' {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
		} else if c == '#' {
			end = i
			break
		}
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(header[:end]), "]"))
	inner = strings.TrimSpace(strings.TrimPrefix(inner, "["))
	// Split first two dotted segments, honoring quotes.
	seg1, rest, _ := cutTomlSeg(inner)
	if seg1 != "peers" {
		return false
	}
	seg2, _, _ := cutTomlSeg(rest)
	return seg2 == name
}

// cutTomlSeg cuts one dotted TOML key segment (bare or "quoted").
func cutTomlSeg(s string) (head, tail string, quoted bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if s[0] == '"' || s[0] == '\'' {
		q := s[0]
		for i := 1; i < len(s); i++ {
			if s[i] == q && s[i-1] != '\\' {
				head = s[1:i]
				tail = strings.TrimSpace(strings.TrimPrefix(s[i+1:], "."))
				return head, tail, true
			}
		}
		return s, "", true
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), false
	}
	return s, "", false
}

func peersSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "reload the daemon (peers resync catalogs)",
		RunE: func(_ *cobra.Command, _ []string) error {
			b, code, err := apiCall("POST", "/reload", nil)
			if err != nil {
				return err
			}
			printJSON(b)
			if code >= 400 {
				os.Exit(1)
			}
			return nil
		},
	}
}

// tomlKeyFor quotes a TOML table key when it needs it (mirror importcfg).
func tomlKeyFor(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}

// samePeerURL compares peer URLs ignoring a trailing slash.
func samePeerURL(a, b string) bool {
	return strings.TrimSuffix(strings.TrimSpace(a), "/") == strings.TrimSuffix(strings.TrimSpace(b), "/")
}
