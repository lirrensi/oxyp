package remhttp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testPKI struct {
	caPEM  []byte
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
}

func mkCA(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caKey: key, caCert: cert}
}

func (p *testPKI) serverCert(t *testing.T, hosts []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func (p *testPKI) clientCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "client"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTransportTLSConfigZero(t *testing.T) {
	if cfg := (TransportTLS{}).config(); cfg != nil {
		t.Fatalf("zero config must be nil: %+v", cfg)
	}
}

func TestTransportTLSConfigMinVersion(t *testing.T) {
	cfg := (TransportTLS{Insecure: true}).config()
	if cfg == nil {
		t.Fatal("want non-nil")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("min version: %x", cfg.MinVersion)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("want InsecureSkipVerify")
	}
	if cfg.RootCAs != nil || len(cfg.Certificates) != 0 {
		t.Fatal("insecure-only must not set CAs or certs")
	}
}

func TestTransportTLSCustomCA(t *testing.T) {
	pki := mkCA(t)
	caPath := writeFile(t, "ca.pem", pki.caPEM)
	cfg := (TransportTLS{CAFile: caPath}).config()
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("want non-nil config with RootCAs")
	}
	srvCert, srvKey := pki.serverCert(t, []string{"127.0.0.1", "localhost"})
	srvCertPath := writeFile(t, "srv.pem", srvCert)
	_ = srvCertPath
	// End-to-end: custom CA client succeeds against a server chained to it.
	cert, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	c := NewClient(5*time.Second, TransportTLS{CAFile: caPath})
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("custom CA end-to-end: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	// Default client (no CA) must use system roots: nil TLSClientConfig.
	plain := NewClient(5*time.Second, TransportTLS{})
	if plain.Transport.(*http.Transport).TLSClientConfig != nil {
		t.Fatal("plain client must use system defaults (nil)")
	}

}

func TestTransportTLSBadCAPEMIgnored(t *testing.T) {
	// Current behavior: unreadable/non-PEM CA content is silently swallowed
	// (tls.go config), yielding a non-nil config with system roots; the
	// connection then fails on unknown authority.
	bad := writeFile(t, "bad.pem", []byte("not a pem\n"))
	cfg := (TransportTLS{CAFile: bad}).config()
	if cfg == nil {
		t.Fatal("want non-nil config even for bad PEM (current behavior)")
	}
}

func TestTransportTLSMTLS(t *testing.T) {
	pki := mkCA(t)
	caPath := writeFile(t, "ca.pem", pki.caPEM)
	cliCert, cliKey := pki.clientCert(t)
	cliCertPath := writeFile(t, "cli.pem", cliCert)
	cliKeyPath := writeFile(t, "cli.key", cliKey)

	cfg := (TransportTLS{CAFile: caPath, CertFile: cliCertPath, KeyFile: cliKeyPath}).config()
	if cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("mtls config: %+v", cfg)
	}
	// End-to-end with RequireAndVerifyClientCert.
	srvCert, srvKey := pki.serverCert(t, []string{"127.0.0.1", "localhost"})
	sc, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pki.caPEM)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{sc},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	srv.StartTLS()
	defer srv.Close()

	good := NewClient(5*time.Second, TransportTLS{CAFile: caPath, CertFile: cliCertPath, KeyFile: cliKeyPath})
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatalf("mtls success: %v", err)
	}
	resp.Body.Close()

	nocert := NewClient(5*time.Second, TransportTLS{CAFile: caPath})
	if _, err := nocert.Get(srv.URL); err == nil {
		t.Fatal("mtls server must reject cert-less client")
	}

	// Cert-only (no key): LoadX509KeyPair error swallowed → 0 certs.
	certOnly := writeFile(t, "only.pem", cliCert)
	cfgOnly := (TransportTLS{CertFile: certOnly, KeyFile: cliKeyPath}).config()
	_ = cfgOnly
	badPair := (TransportTLS{CertFile: certOnly, KeyFile: writeFile(t, "k", []byte("junk"))}).config()
	if badPair == nil || len(badPair.Certificates) != 0 {
		t.Fatalf("cert-only must yield 0 certs (current behavior): %+v", badPair)
	}
}

func TestValidateTLSRealFiles(t *testing.T) {
	pki := mkCA(t)
	caPath := writeFile(t, "ca.pem", pki.caPEM)
	srvCert, srvKey := pki.serverCert(t, []string{"localhost"})
	certPath := writeFile(t, "c.pem", srvCert)
	keyPath := writeFile(t, "k.key", srvKey)
	if got := ValidateTLS("s", caPath, certPath, keyPath); got != "" {
		t.Fatalf("valid: %q", got)
	}
	otherCert, otherKey := pki.serverCert(t, []string{"other"})
	otherCertPath := writeFile(t, "o.pem", otherCert)
	otherKeyPath := writeFile(t, "o.key", otherKey)
	if got := ValidateTLS("s", caPath, otherCertPath, otherKeyPath); got != "" {
		t.Fatalf("second pair valid: %q", got)
	}
	// Swapped cert/key fails.
	if got := ValidateTLS("s", caPath, keyPath, certPath); got == "" {
		t.Fatal("swapped cert/key must fail")
	}
	// Directory paths fail.
	if got := ValidateTLS("s", t.TempDir(), "", ""); got == "" {
		t.Fatal("dir CA must fail")
	}
	// Key without cert fails.
	if got := ValidateTLS("s", "", "", keyPath); got == "" {
		t.Fatal("key-without-cert must fail")
	}
	// Garbage CA PEM passes (documents: no PEM-content validation).
	garbage := writeFile(t, "g.pem", []byte("garbage\n"))
	if got := ValidateTLS("s", garbage, "", ""); got != "" {
		t.Fatalf("garbage CA currently passes: %q", got)
	}
	_ = otherCertPath
	_ = otherKeyPath
}

func TestNewClientInsecureEndToEnd(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	sec := NewClient(5*time.Second, TransportTLS{Insecure: true})
	resp, err := sec.Get(srv.URL)
	if err != nil {
		t.Fatalf("insecure: %v", err)
	}
	resp.Body.Close()
	strict := NewClient(5*time.Second, TransportTLS{})
	if _, err := strict.Get(srv.URL); err == nil {
		t.Fatal("strict must fail on self-signed")
	}
}
