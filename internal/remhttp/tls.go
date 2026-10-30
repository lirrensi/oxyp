package remhttp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TransportTLS is per-server TLS config. No donor implements this
// (lazy-mcp: Node defaults; mcpd: nothing) — all new for oxyp.
type TransportTLS struct {
	CAFile string
	// CertFile/KeyFile enable mTLS. Both or neither.
	CertFile string
	KeyFile  string
	// Insecure skips verification (self-signed dev servers). Never default.
	Insecure bool
}

func (t TransportTLS) config() *tls.Config {
	if t.CAFile == "" && t.CertFile == "" && !t.Insecure {
		return nil // system defaults
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // explicit min
	if t.Insecure {
		cfg.InsecureSkipVerify = true //nolint:gosec // user-configured
	}
	if t.CAFile != "" {
		if pem, err := os.ReadFile(t.CAFile); err == nil {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			pool.AppendCertsFromPEM(pem)
			cfg.RootCAs = pool
		}
	}
	if t.CertFile != "" && t.KeyFile != "" {
		if cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile); err == nil {
			cfg.Certificates = []tls.Certificate{cert}
		}
	}
	return cfg
}

// ValidateTLS checks the TLS file references exist and pair correctly.
// Returns "" on success (config-soft-fail style).
func ValidateTLS(server, caFile, certFile, keyFile string) string {
	if (certFile == "") != (keyFile == "") {
		return fmt.Sprintf("server %q: tls cert and key must both be set", server)
	}
	for _, f := range []string{caFile, certFile, keyFile} {
		if f != "" {
			if st, err := os.Stat(f); err != nil || st.IsDir() {
				return fmt.Sprintf("server %q: tls file unreadable: %s", server, f)
			}
		}
	}
	if certFile != "" {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return fmt.Sprintf("server %q: bad tls cert/key: %v", server, err)
		}
	}
	return ""
}
