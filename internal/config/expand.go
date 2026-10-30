package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	envRefRe  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	fileRefRe = regexp.MustCompile(`\{file:([^}]+)\}`)
)

// expandValue resolves ${VAR} and {file:/abs/path} references.
// ${VAR} with unset VAR keeps the literal (lazy-mcp behavior for headers —
// caller decides fail-closed). {file:...} with unreadable file errors.
func expandValue(v string) (string, error) {
	var firstErr error
	out := fileRefRe.ReplaceAllStringFunc(v, func(m string) string {
		sub := fileRefRe.FindStringSubmatch(m)
		p := sub[1]
		if strings.HasPrefix(p, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				firstErr = fmt.Errorf("{file:%s}: no home", p)
				return m
			}
			p = filepath.Join(home, p[2:])
		}
		if !filepath.IsAbs(p) {
			firstErr = fmt.Errorf("{file:%s}: only absolute paths", p)
			return m
		}
		b, err := os.ReadFile(p)
		if err != nil {
			firstErr = fmt.Errorf("{file:%s}: %w", p, err)
			return m
		}
		return strings.TrimRight(string(b), "\r\n")
	})
	if firstErr != nil {
		return "", firstErr
	}
	out = envRefRe.ReplaceAllStringFunc(out, func(m string) string {
		name := envRefRe.FindStringSubmatch(m)[1]
		if val, ok := os.LookupEnv(name); ok {
			return val
		}
		return m // keep literal; validated separately for remote fail-closed
	})
	return out, nil
}

// expandServerSecrets expands header/env values. Remote entries fail closed:
// any leftover ${VAR} in url/headers marks ConfigError (hermes behavior).
// Local env keeps lazy-mcp behavior (literal passthrough). File errors always
// mark ConfigError.
func expandServerSecrets(s *ServerEntry) string {
	if s.OAuth != nil {
		for k, v := range map[string]string{
			"client_id": s.OAuth.ClientID, "client_secret": s.OAuth.ClientSecret,
			"scope": s.OAuth.Scope, "redirect_uri": s.OAuth.RedirectURI,
		} {
			if v == "" {
				continue
			}
			ev, err := expandValue(v)
			if err != nil {
				return fmt.Sprintf("server %q: oauth.%s: %v", s.Name, k, err)
			}
			if envRefRe.MatchString(ev) || fileRefRe.MatchString(ev) {
				return fmt.Sprintf("server %q: oauth.%s has unset ${VAR}", s.Name, k)
			}
			switch k {
			case "client_id":
				s.OAuth.ClientID = ev
			case "client_secret":
				s.OAuth.ClientSecret = ev
			case "scope":
				s.OAuth.Scope = ev
			case "redirect_uri":
				s.OAuth.RedirectURI = ev
			}
		}
	}
	if s.URL != "" {
		if u, err := expandValue(s.URL); err != nil {
			return fmt.Sprintf("server %q: url: %v", s.Name, err)
		} else if envRefRe.MatchString(u) {
			return fmt.Sprintf("server %q: url has unset ${VAR}", s.Name)
		} else {
			s.URL = u
		}
		for k, v := range s.Headers {
			ev, err := expandValue(v)
			if err != nil {
				return fmt.Sprintf("server %q: headers.%s: %v", s.Name, k, err)
			}
			if envRefRe.MatchString(ev) {
				return fmt.Sprintf("server %q: headers.%s has unset ${VAR}", s.Name, k)
			}
			s.Headers[k] = ev
		}
	}
	for k, v := range s.Env {
		if ev, err := expandValue(v); err != nil {
			return fmt.Sprintf("server %q: env.%s: %v", s.Name, k, err)
		} else {
			s.Env[k] = ev
		}
	}
	return ""
}
