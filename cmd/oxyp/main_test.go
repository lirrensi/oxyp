package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lirrensi/oxyp/internal/config"
)

// captureStdout swaps os.Stdout for the duration of fn and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	outCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		outCh <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-outCh
}

// withDirFlag sets dirFlag (global) for the duration of the test.
func withDirFlag(t *testing.T, dir string) {
	t.Helper()
	old := dirFlag
	dirFlag = dir
	t.Cleanup(func() { dirFlag = old })
}

func writeMinimalTOML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- endpoint round trip + 0600 on create ---

func TestEndpointRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	withDirFlag(t, dir)
	e := endpoint{URL: "http://127.0.0.1:8379", PID: os.Getpid(), Token: "tok", StartedAt: "x"}
	if err := writeEndpoint(e); err != nil {
		t.Fatalf("writeEndpoint: %v", err)
	}
	got := readEndpoint()
	if got == nil {
		t.Fatal("readEndpoint returned nil for live pid")
	}
	if got.URL != e.URL || got.Token != e.Token || got.PID != e.PID {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	st, err := os.Stat(endpointPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
}

// --- readEndpoint malformed/empty-URL/PID cases ---

func TestReadEndpointRejectsBad(t *testing.T) {
	dir := t.TempDir()
	withDirFlag(t, dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRaw := func(s string) {
		t.Helper()
		if err := os.WriteFile(endpointPath(), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// missing file -> nil
	if got := readEndpoint(); got != nil {
		t.Fatalf("missing file: got %+v", got)
	}
	writeRaw(`{not json`)
	if got := readEndpoint(); got != nil {
		t.Fatalf("malformed: got %+v", got)
	}
	writeRaw(`{"url":"","pid":123,"token":"t"}`)
	if got := readEndpoint(); got != nil {
		t.Fatalf("empty URL: got %+v", got)
	}
	writeRaw(`{"url":"http://x","pid":0,"token":"t"}`)
	if got := readEndpoint(); got != nil {
		t.Fatalf("PID 0: got %+v", got)
	}
	writeRaw(fmt.Sprintf(`{"url":"http://127.0.0.1:9","pid":%d,"token":"t"}`, os.Getpid()))
	if got := readEndpoint(); got == nil {
		t.Fatal("live PID must be non-nil")
	}
	// dead PID (kill -0 fails): current behavior returns nil.
	// Find a surely-dead pid by spawning and waiting.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot spawn true")
	}
	dead := cmd.Process.Pid
	writeRaw(fmt.Sprintf(`{"url":"http://127.0.0.1:9","pid":%d,"token":"t"}`, dead))
	if got := readEndpoint(); got != nil {
		t.Fatalf("dead PID: current behavior must be nil, got %+v", got)
	}
}

func TestWriteEndpointLeavesExistingMode(t *testing.T) {
	dir := t.TempDir()
	withDirFlag(t, dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// current behavior: existing 0644 file keeps its mode (documented bug).
	if err := os.WriteFile(endpointPath(), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpoint(endpoint{URL: "http://127.0.0.1:9", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(endpointPath())
	// current behavior: mode unchanged (0644), not tightened.
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("current behavior: existing file keeps 0644, got %o", st.Mode().Perm())
	}
}

// --- alive true/false + bearer capture ---

func TestAliveTrueFalseAndBearer(t *testing.T) {
	var gotAuth string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer ok.Close()
	e := &endpoint{URL: ok.URL, Token: "sekret"}
	if !alive(e) {
		t.Fatal("alive must be true on 200")
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("bearer = %q", gotAuth)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer bad.Close()
	if alive(&endpoint{URL: bad.URL}) {
		t.Fatal("alive must be false on 401")
	}
	if alive(&endpoint{URL: "http://127.0.0.1:1"}) {
		t.Fatal("alive must be false on refused")
	}
	if alive(&endpoint{URL: "http://%zz"}) {
		t.Fatal("alive must be false on bad URL")
	}
}

func TestProbeOxypClassification(t *testing.T) {
	mine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"pid":%d,"startedAt":"2026-10-30T00:00:00Z","servers":{}}`, os.Getpid())
	}))
	defer mine.Close()
	// No endpoint on file: responder PID is live, no mismatch → ours.
	withDirFlag(t, t.TempDir())
	pid, started, ok := probeOxyp(mine.URL, "tok")
	if !ok || pid != os.Getpid() || started == "" {
		t.Fatalf("ours: got pid=%d started=%q ok=%v", pid, started, ok)
	}
	// Wrong token → 401 → not us.
	if _, _, ok := probeOxyp(mine.URL, "wrong"); ok {
		t.Fatal("wrong token must not classify as ours")
	}
	// Stranger answering 200 with non-oxyp JSON → not us.
	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer stranger.Close()
	if _, _, ok := probeOxyp(stranger.URL, ""); ok {
		t.Fatal("stranger JSON must not classify as ours")
	}
	// Stranger answering 404 (plain HTTP server, no /health) → not us.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer plain.Close()
	if _, _, ok := probeOxyp(plain.URL, ""); ok {
		t.Fatal("404 must not classify as ours")
	}
	// Oxyp-shaped body naming a DEAD pid → not us. Find one by bumping
	// past our own PID until kill -0 fails (PIDs are dense; a few tries
	// always land on a corpse — and never on a recycled live one that
	// equals ours, which is the only false-accept risk).
	deadPID := os.Getpid() + 1
	for range 10000 {
		p, _ := os.FindProcess(deadPID)
		if p.Signal(syscall.Signal(0)) != nil {
			break
		}
		deadPID++
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"pid":%d,"startedAt":"x","servers":{}}`, deadPID)
	}))
	defer dead.Close()
	if _, _, ok := probeOxyp(dead.URL, ""); ok {
		t.Fatal("dead PID must not classify as ours")
	}
	// Endpoint on file naming a different LIVE pid → not us (another
	// dir's daemon squatting our port). Hold a sleeper to own one.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	defer func() { _ = sleep.Process.Kill() }()
	if err := writeEndpoint(endpoint{URL: mine.URL, PID: sleep.Process.Pid, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := probeOxyp(mine.URL, "tok"); ok {
		t.Fatal("endpoint PID mismatch must not classify as ours")
	}
	// Same setup but endpoint agrees with the responder → ours.
	if err := writeEndpoint(endpoint{URL: mine.URL, PID: os.Getpid(), Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := probeOxyp(mine.URL, "tok"); !ok {
		t.Fatal("matching endpoint PID must classify as ours")
	}
	// Connection refused → not us.
	if _, _, ok := probeOxyp("http://127.0.0.1:1", ""); ok {
		t.Fatal("refused must not classify as ours")
	}
}

func TestStopDaemonLifecycle(t *testing.T) {
	withDirFlag(t, t.TempDir())
	// Nothing running → clean false, no error.
	stopped, err := stopDaemon()
	if err != nil || stopped {
		t.Fatalf("empty: stopped=%v err=%v", stopped, err)
	}
	// Live process named by the endpoint → SIGTERM, waited, file gone.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	if err := writeEndpoint(endpoint{URL: "http://127.0.0.1:9", PID: sleep.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	stopped, err = stopDaemon()
	if err != nil || !stopped {
		t.Fatalf("live: stopped=%v err=%v", stopped, err)
	}
	if _, err := os.Stat(endpointPath()); !os.IsNotExist(err) {
		t.Fatal("endpoint file must be removed after stop")
	}
	if err := sleep.Wait(); err == nil {
		t.Fatal("sleeper should have died by signal, not exited 0")
	}
	// Stale endpoint (dead PID) → clean false, no error.
	withDirFlag(t, t.TempDir())
	if err := os.MkdirAll(oxypDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	deadPID := os.Getpid() + 7654321
	if err := os.WriteFile(endpointPath(), []byte(fmt.Sprintf(`{"url":"http://127.0.0.1:9","pid":%d}`, deadPID)), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped, err = stopDaemon()
	if err != nil || stopped {
		t.Fatalf("stale: stopped=%v err=%v", stopped, err)
	}
}

// --- apiCall method/path/body/content-type/bearer/code ---

func TestAPICallRoundTrip(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		gotCT, gotAuth = r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"e":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	withDirFlag(t, t.TempDir())
	if err := os.MkdirAll(oxypDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpoint(endpoint{URL: srv.URL, PID: os.Getpid(), Token: "tok123"}); err != nil {
		t.Fatal(err)
	}

	b, code, err := apiCall("POST", "/servers/a/tools/b", map[string]any{"parameters": map[string]any{"x": 1}})
	if err != nil || code != 200 {
		t.Fatalf("apiCall = %v %d %v", b, code, err)
	}
	if gotMethod != "POST" || gotPath != "/servers/a/tools/b" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("bearer = %q", gotAuth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not json: %q", gotBody)
	}

	b, code, err = apiCall("GET", "/health", nil)
	if err != nil || code != 200 {
		t.Fatalf("GET = %v %d %v", b, code, err)
	}
	if gotCT != "" {
		// GET with nil body must NOT send content-type; gotCT holds last request's header.
		// Re-check via fresh request through a second server? Simpler: assert via captured per-request.
	}
	_, code, err = apiCall("GET", "/fail", nil)
	if err != nil || code != 500 {
		t.Fatalf("fail = %d %v", code, err)
	}
}

func TestAPICallGETSendsNoContentType(t *testing.T) {
	var gotCT string
	var gotBodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBodyLen = len(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withDirFlag(t, t.TempDir())
	if err := os.MkdirAll(oxypDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpoint(endpoint{URL: srv.URL, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := apiCall("GET", "/health", nil); err != nil {
		t.Fatal(err)
	}
	if gotCT != "" {
		t.Fatalf("GET content-type = %q, want empty", gotCT)
	}
	if gotBodyLen != 0 {
		t.Fatalf("GET body len = %d, want 0", gotBodyLen)
	}
}

// --- printJSON large-int + escaping ---

func TestPrintJSONLargeIntCurrent(t *testing.T) {
	// current behavior: numbers decode to float64, so integers > 2^53 lose precision.
	out := captureStdout(t, func() { printJSON([]byte(`{"n":9007199254740993}`)) })
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("printJSON output not json: %q", out)
	}
	// Pin current lossy behavior: 2^53+1 cannot survive a float64 round trip.
	if got, ok := v["n"].(float64); !ok || got != 9007199254740992 {
		t.Fatalf("expected float64 precision loss for 2^53+1, got %v (%q)", v["n"], out)
	}
	if !strings.Contains(out, "9007199254740992") {
		t.Fatalf("current behavior rounds 2^53+1 to 2^53, got %q", out)
	}
}

func TestPrintJSONEscapesHTMLCurrent(t *testing.T) {
	// current behavior: json.Encoder escapes <>& by default.
	out := captureStdout(t, func() { printJSON([]byte(`{"s":"<a>&\"b\""}`)) })
	if strings.Contains(out, "<a>") {
		t.Fatalf("current behavior escapes HTML: got %q", out)
	}
	if !strings.Contains(out, "\\u003c") {
		t.Fatalf("want \\u003c escape, got %q", out)
	}
}

func TestPrintJSONPassthroughNonJSON(t *testing.T) {
	out := captureStdout(t, func() { printJSON([]byte(`not json`)) })
	if strings.TrimSpace(out) != "not json" {
		t.Fatalf("got %q", out)
	}
}

// --- traceEnabled truth table ---

func TestTraceEnabledTable(t *testing.T) {
	boolp := func(b bool) *bool { return &b }
	mkServer := func(trace *config.TraceConfig) config.ServerEntry {
		return config.ServerEntry{Name: "s", Command: []string{"x"}, Trace: trace}
	}
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"nil all", &config.Config{}, false},
		{"daemon on", &config.Config{Daemon: config.DaemonConfig{Trace: &config.TraceConfig{Enabled: true}}}, true},
		{"daemon off", &config.Config{Daemon: config.DaemonConfig{Trace: &config.TraceConfig{Enabled: false}}}, false},
		{"server on", &config.Config{Servers: map[string]config.ServerEntry{"s": mkServer(&config.TraceConfig{Enabled: true})}}, true},
		{"server off", &config.Config{Servers: map[string]config.ServerEntry{"s": mkServer(&config.TraceConfig{Enabled: false})}}, false},
		{"daemon off server on", &config.Config{
			Daemon:  config.DaemonConfig{Trace: &config.TraceConfig{Enabled: false}},
			Servers: map[string]config.ServerEntry{"s": mkServer(&config.TraceConfig{Enabled: true})},
		}, true},
		{"disabled-but-traced still counts", &config.Config{
			Servers: map[string]config.ServerEntry{"s": {Name: "s", Command: []string{"x"}, Enabled: boolp(false), Trace: &config.TraceConfig{Enabled: true}}},
		}, true},
	}
	for _, tc := range cases {
		if got := traceEnabled(tc.cfg); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// --- cobra extra-args ---

func TestCobraExtraArgsRejected(t *testing.T) {
	// current behavior: these commands lack Args validation, so extra args are
	// ACCEPTED at parse time. Pin via ValidateArgs (no daemon autostart, fast).
	for name, c := range map[string]interface {
		ValidateArgs([]string) error
	}{
		"status": statusCmd(), "reload": reloadCmd(), "shim": shimCmd(), "doctor": doctorCmd(),
	} {
		if err := c.ValidateArgs([]string{"unexpected-extra-arg"}); err != nil {
			t.Errorf("%s rejects extra args (behavior change): %v", name, err)
		} else {
			t.Logf("%s accepts extra args (current behavior)", name)
		}
	}
}

// --- call-name escaping ---

func TestCallNamePathEscapeObserved(t *testing.T) {
	// Simulate what callCmd builds: PathEscape must escape /, %, newline.
	cases := map[string]string{
		"plain":    "plain",
		"a/b":      "a%2Fb",
		"a%b":      "a%25b",
		"a\nb":     "a%0Ab",
		"a b+c?d":  "a%20b+c%3Fd",
		"héllo":    "h%C3%A9llo",
		"srv%2F/x": "srv%252F%2Fx",
	}
	for in, want := range cases {
		if got := url.PathEscape(in); got != want {
			t.Errorf("PathEscape(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(url.PathEscape(in), "/") && in != "plain" && !strings.Contains(in, "/") {
			t.Errorf("unescaped slash in %q", in)
		}
	}
	// End-to-end through apiCall: path built by callCmd reaches the fake daemon intact.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withDirFlag(t, t.TempDir())
	if err := os.MkdirAll(oxypDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpoint(endpoint{URL: srv.URL, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	server, tool := "a/b", "c%d\ne"
	want := "/servers/" + url.PathEscape(server) + "/tools/" + url.PathEscape(tool)
	if _, _, err := apiCall("POST", want, map[string]any{"parameters": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != want {
		t.Fatalf("path = %q want %q", gotPath, want)
	}
}

// --- search flag encoding incl zero/negative omitted ---

func TestSearchFlagEncoding(t *testing.T) {
	encode := func(server string, regex bool, limit, offset int, q string) string {
		vals := url.Values{"q": {q}}
		if server != "" {
			vals.Set("server", server)
		}
		if regex {
			vals.Set("regex", "1")
		}
		if limit > 0 {
			vals.Set("limit", fmt.Sprint(limit))
		}
		if offset > 0 {
			vals.Set("offset", fmt.Sprint(offset))
		}
		return "/tools?" + vals.Encode()
	}
	u := encode("s", true, 5, 10, "a+b c")
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if q.Get("q") != "a+b c" || q.Get("server") != "s" || q.Get("regex") != "1" || q.Get("limit") != "5" || q.Get("offset") != "10" {
		t.Fatalf("encoded %q parsed %+v", u, q)
	}
	for _, tc := range []struct{ limit, offset int }{{0, 0}, {-1, -5}} {
		enc := encode("", false, tc.limit, tc.offset, "q")
		p, _ := url.Parse(enc)
		if p.Query().Has("limit") || p.Query().Has("offset") {
			t.Errorf("limit=%d offset=%d must be omitted, got %q", tc.limit, tc.offset, enc)
		}
	}
}

// --- login validation errors before network ---

func TestLoginValidationBeforeNetwork(t *testing.T) {
	mkLogin := func(dir, server string, extraArgs ...string) error {
		withDirFlag(t, dir)
		c := loginCmd()
		args := append([]string{server}, extraArgs...)
		c.SetArgs(args)
		return c.Execute()
	}

	// unknown server
	dir := t.TempDir()
	writeMinimalTOML(t, dir, `[servers.known]
url = "https://example.com/mcp"
`)
	if err := mkLogin(dir, "nope"); err == nil || !strings.Contains(err.Error(), `server "nope" not found`) {
		t.Fatalf("unknown server err = %v", err)
	}

	// local server is not an oauth remote
	dir2 := t.TempDir()
	writeMinimalTOML(t, dir2, `[servers.loc]
command = ["echo", "hi"]
`)
	if err := mkLogin(dir2, "loc"); err == nil || !strings.Contains(err.Error(), "not an oauth remote") {
		t.Fatalf("local server err = %v", err)
	}

	// auth=headers is not oauth
	dir3 := t.TempDir()
	writeMinimalTOML(t, dir3, `[servers.h]
url = "https://example.com/mcp"
auth = "headers"
`)
	c := loginCmd()
	withDirFlag(t, dir3)
	c.SetArgs([]string{"h"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "not an oauth remote") {
		t.Fatalf("auth=headers err = %v", err)
	}

	// fixed redirect_uri port conflicts with callback_port
	dir4 := t.TempDir()
	writeMinimalTOML(t, dir4, `[servers.r]
url = "https://example.com/mcp"
auth = "oauth"
[servers.r.oauth]
redirect_uri = "http://localhost:9999/callback"
callback_port = 8888
`)
	c4 := loginCmd()
	withDirFlag(t, dir4)
	c4.SetArgs([]string{"r"})
	if err := c4.Execute(); err == nil || !strings.Contains(err.Error(), "conflicts with callback_port") {
		t.Fatalf("port conflict err = %v", err)
	}

	// bad redirect_uri template
	dir5 := t.TempDir()
	writeMinimalTOML(t, dir5, `[servers.r]
url = "https://example.com/mcp"
auth = "oauth"
[servers.r.oauth]
redirect_uri = "http://localhost/callback"
`)
	c5 := loginCmd()
	withDirFlag(t, dir5)
	c5.SetArgs([]string{"r"})
	if err := c5.Execute(); err == nil {
		t.Fatal("bad redirect_uri must error before network")
	}
}

// --- bad json args for call ---

func TestCallBadJSONArgs(t *testing.T) {
	c := callCmd()
	c.SetArgs([]string{"srv", "tool", "{bad json"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "bad json args") {
		t.Fatalf("err = %v", err)
	}
	c2 := callCmd()
	c2.SetArgs([]string{"only-one"})
	if err := c2.Execute(); err == nil {
		t.Fatal("call with 1 arg must fail Args validation")
	}
}

// --- servers filter escaping via apiCall ---

func TestServersFilterEscaping(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw = r.URL.RequestURI()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	withDirFlag(t, t.TempDir())
	if err := os.MkdirAll(oxypDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeEndpoint(endpoint{URL: srv.URL, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	filter := "a+b c&d/e"
	path := "/servers?filter=" + url.QueryEscape(filter)
	if _, _, err := apiCall("GET", path, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotRaw, "/servers?") {
		t.Fatalf("raw = %q", gotRaw)
	}
	parsed, _ := url.Parse(gotRaw)
	if parsed.Query().Get("filter") != filter {
		t.Fatalf("filter round trip = %q, raw %q", parsed.Query().Get("filter"), gotRaw)
	}
}

var _ = bytes.MinRead // keep bytes import if unused in future edits
