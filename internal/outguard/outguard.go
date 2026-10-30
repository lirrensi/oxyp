// Package outguard caps TEXT result size when explicitly armed.
// Media blocks (image/audio/resource) are NEVER touched — truncating
// base64 corrupts it. Oversized text is cut to a head preview and the
// full text spills to a 0600 temp file whose path rides in the result.
package outguard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// Limits mirrors config.OutputGuardConfig (uncoupled for tests).
type Limits struct {
	MaxBytes int
	MaxLines int
}

// Apply walks content blocks: text over either limit is replaced by a
// head preview + spill-file note. Returns the (possibly new) slice and
// whether anything spilled. Non-text blocks pass through untouched.
func Apply(blocks []mcp.Content, lim Limits) ([]mcp.Content, bool) {
	// Negative limits are meaningless (config validation rejects them,
	// but defense in depth): clamp to 0 = spill everything.
	lim.MaxBytes = max(lim.MaxBytes, 0)
	lim.MaxLines = max(lim.MaxLines, 0)
	out := make([]mcp.Content, len(blocks))
	spilled := false
	for i, b := range blocks {
		tc, ok := b.(mcp.TextContent)
		if !ok {
			out[i] = b
			continue
		}
		if len(tc.Text) <= lim.MaxBytes && countLines(tc.Text) <= lim.MaxLines {
			out[i] = b
			continue
		}
		preview, skipped := head(tc.Text, lim)
		path, err := spill(tc.Text)
		note := fmt.Sprintf("\n…[output guard: showing head, %d bytes skipped", skipped)
		if err != nil {
			note += "; spill failed: " + err.Error()
		} else {
			note += "; full output in " + path
		}
		note += "]"
		tc.Text = preview + note
		out[i] = tc
		spilled = true
	}
	return out, spilled
}

// countLines counts content lines: a trailing newline terminates the last
// line instead of starting an empty one ("a\nb\nc\n" is 3 lines).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// head cuts s to fit both limits on UTF-8 + line boundaries.
// Returns preview and skipped byte count.
func head(s string, lim Limits) (string, int) {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > lim.MaxLines {
		lines = lines[:lim.MaxLines]
	}
	out := strings.Join(lines, "")
	if len(out) > lim.MaxBytes {
		out = out[:lim.MaxBytes]
		for len(out) > 0 && !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
	}
	return out, len(s) - len(out)
}

func spill(text string) (string, error) {
	f, err := os.CreateTemp("", "oxyp-output-*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	// CreateTemp is 0600 already; enforce explicitly (umask paranoia).
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	pruneSpills(filepath.Dir(path))
	return path, nil
}

// Retention for spill files in the temp dir: newest 100 files,
// 100 MiB total, 24h max age. Best-effort: errors are ignored.
const (
	maxSpillFiles = 100
	maxSpillBytes = 100 << 20
	maxSpillAge   = 24 * time.Hour
)

// pruneSpills removes old oxyp-output-* files beyond retention.
func pruneSpills(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fi struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fi
	now := time.Now()
	cutoff := now.Add(-maxSpillAge)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "oxyp-output-") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		p := filepath.Join(dir, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if st.ModTime().Before(cutoff) {
			_ = os.Remove(p)
			continue
		}
		files = append(files, fi{p, st.Size(), st.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	var total int64
	for i, f := range files {
		total += f.size
		if i >= maxSpillFiles || total > maxSpillBytes {
			_ = os.Remove(f.path)
		}
	}
}
