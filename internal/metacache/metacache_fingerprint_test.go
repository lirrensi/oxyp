package metacache

import (
	"testing"

	"oxyp/internal/config"
)

// Fingerprint matrix, split out per the assignment: every identity-relevant
// field flips the hash, secrets stay out, ordering is stable.
func TestFingerprintCommandAndURL(t *testing.T) {
	a := config.ServerEntry{Command: []string{"bin", "--x"}}
	b := config.ServerEntry{Command: []string{"bin", "--y"}}
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("argv change must flip fingerprint")
	}
	c := config.ServerEntry{Command: []string{"bin"}}
	if Fingerprint(a) == Fingerprint(c) {
		t.Fatal("argv length change must flip fingerprint")
	}
	u1 := config.ServerEntry{URL: "https://h/one"}
	u2 := config.ServerEntry{URL: "https://h/two"}
	if Fingerprint(u1) == Fingerprint(u2) {
		t.Fatal("url change must flip fingerprint")
	}
	t1 := config.ServerEntry{URL: "https://h/m"}
	t2 := t1
	t2.Transport = "sse"
	if Fingerprint(t1) == Fingerprint(t2) {
		t.Fatal("transport change must flip fingerprint")
	}
}

func TestFingerprintEnvHeadersCwdAuth(t *testing.T) {
	base := config.ServerEntry{Command: []string{"b"}}
	mk := func(mut func(*config.ServerEntry)) string {
		e := base
		mut(&e)
		return Fingerprint(e)
	}
	fp := Fingerprint(base)
	if mk(func(e *config.ServerEntry) { e.Headers = map[string]string{"H": "1"} }) == fp {
		t.Error("header add must flip")
	}
	if mk(func(e *config.ServerEntry) { e.Env = map[string]string{"E": "1"} }) == fp {
		t.Error("env add must flip")
	}
	if mk(func(e *config.ServerEntry) { e.Cwd = "/x" }) == fp {
		t.Error("cwd must flip")
	}
	if mk(func(e *config.ServerEntry) { e.Auth = "oauth" }) == fp {
		t.Error("auth must flip")
	}
	inhT := true
	if mk(func(e *config.ServerEntry) { e.InheritEnv = &inhT }) == fp {
		t.Error("inherit_env must flip")
	}
}

func TestFingerprintTLSSecretExclusion(t *testing.T) {
	base := config.ServerEntry{Command: []string{"b"}}
	withTLS := base
	withTLS.TLS = &config.TLSConfig{CAFile: "ca", CertFile: "c", KeyFile: "k", Insecure: true}
	if Fingerprint(base) == Fingerprint(withTLS) {
		t.Fatal("TLS settings must affect fingerprint")
	}
	o1 := base
	o1.OAuth = &config.OAuthConfig{ClientID: "id", ClientSecret: "one", CallbackPort: 1, Flow: "browser", Timeout: 9}
	o2 := base
	o2.OAuth = &config.OAuthConfig{ClientID: "id", ClientSecret: "two", CallbackPort: 1, Flow: "browser", Timeout: 9}
	if Fingerprint(o1) != Fingerprint(o2) {
		t.Fatal("client_secret rotation must NOT invalidate the cache")
	}
	for _, mut := range []func(*config.OAuthConfig){
		func(o *config.OAuthConfig) { o.ClientID = "other" },
		func(o *config.OAuthConfig) { o.CallbackPort = 2 },
		func(o *config.OAuthConfig) { o.Flow = "device" },
		func(o *config.OAuthConfig) { o.Timeout = 10 },
		func(o *config.OAuthConfig) { o.Scope = "s" },
	} {
		alt := o1
		cp := *alt.OAuth
		mut(&cp)
		alt.OAuth = &cp
		if Fingerprint(o1) == Fingerprint(alt) {
			t.Error("oauth identity field change must flip fingerprint")
		}
	}
}
func TestFingerprintIgnoresNonIdentity(t *testing.T) {
	base := config.ServerEntry{Command: []string{"b"}, Description: "x", Lifecycle: "eager"}
	for _, mut := range []func(*config.ServerEntry){
		func(e *config.ServerEntry) { e.Lifecycle = "keep-alive" },
		func(e *config.ServerEntry) { e.Description = "y" },
		func(e *config.ServerEntry) { e.Tools = nil },
		func(e *config.ServerEntry) { e.IdleTimeout = new(3) },
		func(e *config.ServerEntry) { e.OutputGuard = nil },
		func(e *config.ServerEntry) { e.Trace = nil },
	} {
		e := base
		mut(&e)
		if Fingerprint(e) != Fingerprint(base) {
			t.Error("non-identity field flipped fingerprint")
		}
	}
}

func intPtr(i int) *int { return &i }
