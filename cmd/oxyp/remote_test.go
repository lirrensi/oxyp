package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- remote endpoint parsing ---

func TestRemoteEndpointParsing(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		token   string
		wantURL string
		wantErr string
	}{
		{"unset", "", "", "", ""},
		{"blank", "   ", "", "", ""},
		{"plain host+port", "http://192.168.1.5:8379", "sekret", "http://192.168.1.5:8379", ""},
		{"https ok", "https://oxyp.lan:8379", "t", "https://oxyp.lan:8379", ""},
		{"trailing slash stripped", "http://h:1/", "", "http://h:1", ""},
		{"path/query/fragment stripped", "http://h:1/mcp?x=1#frag", "", "http://h:1", ""},
		{"surrounding spaces trimmed", "  http://h:1  ", "  tok  ", "http://h:1", ""},
		{"missing scheme", "192.168.1.5:8379", "", "", "bad OXYP_URL"},
		{"bad scheme", "ftp://h/mcp", "", "", "bad OXYP_URL"},
		{"no host", "http:///mcp", "", "", "bad OXYP_URL"},
		{"userinfo rejected", "http://user:pass@h:1", "", "", "userinfo"},
		{"unparseable", "http://%zz", "", "", "bad OXYP_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OXYP_URL", tc.url)
			t.Setenv("OXYP_TOKEN", tc.token)
			e, err := remoteEndpoint()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.wantURL == "" {
				if e != nil {
					t.Fatalf("unset must give nil, got %+v", e)
				}
				return
			}
			if e == nil || e.URL != tc.wantURL {
				t.Fatalf("URL = %+v, want %q", e, tc.wantURL)
			}
			if !e.isRemote() || !e.Remote {
				t.Fatalf("must be flagged remote: %+v", e)
			}
			if e.Token != strings.TrimSpace(tc.token) {
				t.Fatalf("token = %q", e.Token)
			}
		})
	}
}

func TestRemoteTokenTrimsPadding(t *testing.T) {
	t.Setenv("OXYP_URL", "http://h:1")
	t.Setenv("OXYP_TOKEN", "  padded  ")
	e, err := remoteEndpoint()
	if err != nil || e == nil {
		t.Fatalf("endpoint = %+v err = %v", e, err)
	}
	if e.Token != "padded" {
		t.Fatalf("token = %q, want trimmed", e.Token)
	}
}

// --- localDialURL ---

func TestLocalDialURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:8379", "http://127.0.0.1:8379"},
		{"[::1]:8379", "http://[::1]:8379"},
		{"localhost:8379", "http://localhost:8379"},
		{"192.168.1.5:8379", "http://192.168.1.5:8379"},
		{"0.0.0.0:8379", "http://127.0.0.1:8379"},
		{"[::]:8379", "http://127.0.0.1:8379"},
		{":8379", "http://127.0.0.1:8379"},
	}
	for _, tc := range cases {
		if got := localDialURL(tc.in); got != tc.want {
			t.Errorf("localDialURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- ensureDaemon remote behavior ---

func TestEnsureDaemonRemote(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"servers":{}}`))
	}))
	defer srv.Close()

	withDirFlag(t, t.TempDir())
	t.Setenv("OXYP_URL", srv.URL)
	t.Setenv("OXYP_TOKEN", "sekret")

	e, err := ensureDaemon()
	if err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if !e.isRemote() || e.URL != srv.URL || e.Token != "sekret" {
		t.Fatalf("endpoint = %+v", e)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("alive bearer = %q", gotAuth)
	}
}

func TestEnsureDaemonRemoteNoAutostart(t *testing.T) {
	// Refused port: remote must fail fast, never spawn a local daemon.
	withDirFlag(t, t.TempDir())
	t.Setenv("OXYP_URL", "http://127.0.0.1:1")
	t.Setenv("OXYP_TOKEN", "")
	if _, err := ensureDaemon(); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want unreachable", err)
	}
}

func TestEnsureDaemonBadURLFailsClosed(t *testing.T) {
	// Set-but-invalid OXYP_URL must error, never fall back to local.
	withDirFlag(t, t.TempDir())
	t.Setenv("OXYP_URL", "notaurl")
	if _, err := ensureDaemon(); err == nil || !strings.Contains(err.Error(), "bad OXYP_URL") {
		t.Fatalf("err = %v, want bad OXYP_URL", err)
	}
}

func TestAPICallRemoteBearer(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	withDirFlag(t, t.TempDir())
	t.Setenv("OXYP_URL", srv.URL+"/ignored-path?x=1")
	t.Setenv("OXYP_TOKEN", "tok123")

	b, code, err := apiCall("GET", "/health", nil)
	if err != nil || code != 200 {
		t.Fatalf("apiCall = %s %d %v", b, code, err)
	}
	if gotAuth != "Bearer tok123" || gotPath != "/health" {
		t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
	}
}

// --- local-only guards ---

func TestRequireLocal(t *testing.T) {
	t.Setenv("OXYP_URL", "")
	if err := requireLocal("stop"); err != nil {
		t.Fatalf("unset must pass: %v", err)
	}
	t.Setenv("OXYP_URL", "http://h:1")
	if err := requireLocal("stop"); err == nil || !strings.Contains(err.Error(), "local-only") {
		t.Fatalf("err = %v, want local-only", err)
	}
}

func TestStopDaemonRefusesRemote(t *testing.T) {
	withDirFlag(t, t.TempDir())
	t.Setenv("OXYP_URL", "http://h:1")
	if _, err := stopDaemon(); err == nil || !strings.Contains(err.Error(), "OXYP_URL") {
		t.Fatalf("err = %v, want remote refusal", err)
	}
}

func TestLocalOnlyCommandsRefuseRemote(t *testing.T) {
	withDirFlag(t, t.TempDir())
	t.Setenv("OXYP_URL", "http://h:1")
	t.Setenv("OXYP_TOKEN", "t")
	for name, cmd := range map[string]interface{ Execute() error }{
		"daemon": daemonCmd(),
		"login":  loginCmd(),
		"doctor": doctorCmd(),
		"add":    addCmd(),
		"import": importCmd(),
		"stop":   stopCmd(),
		"restart": restartCmd(),
	} {
		if name == "login" {
			cmd.(interface{ SetArgs([]string) }).SetArgs([]string{"x"})
		}
		if name == "add" {
			cmd.(interface{ SetArgs([]string) }).SetArgs([]string{"http://x/mcp"})
		}
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "local-only") && !strings.Contains(err.Error(), "OXYP_URL") {
			t.Fatalf("%s: err = %v, want local-only refusal", name, err)
		}
	}
}
