package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPeerHeader(t *testing.T) {
	cases := []struct {
		header string
		name   string
		want   bool
	}{
		{"[peers.wsl]", "wsl", true},
		{`[peers."wsl"]`, "wsl", true},
		{"[peers.wsl.servers.github]", "wsl", true},
		{"[peers.other]", "wsl", false},
		{"[servers.wsl]", "wsl", false},
		{"[daemon]", "wsl", false},
		{"[peers.wslx]", "wsl", false},
		{"[peers.wsl-x]", "wsl", false},
	}
	for _, tc := range cases {
		if got := isPeerHeader(tc.header, tc.name); got != tc.want {
			t.Errorf("%s vs %q = %v, want %v", tc.header, tc.name, got, tc.want)
		}
	}
}

func TestRemovePeerBlock(t *testing.T) {
	dir := t.TempDir()
	orig := `[daemon]
addr = "127.0.0.1:8379"

[servers.a]
command = ["true"]

[peers.wsl]
url = "http://127.0.0.1:9"

[peers.wsl.servers.g]
priority = -1

[peers.other]
url = "http://127.0.0.1:8"
`
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePeerBlock(dir, "wsl"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "oxyp.toml"))
	if strings.Contains(string(got), "peers.wsl") || strings.Contains(string(got), "127.0.0.1:9") {
		t.Fatalf("wsl block must be gone:\n%s", got)
	}
	for _, keep := range []string{"[servers.a]", "[peers.other]", "127.0.0.1:8"} {
		if !strings.Contains(string(got), keep) {
			t.Fatalf("must keep %q:\n%s", keep, got)
		}
	}
}

func TestSamePeerURL(t *testing.T) {
	if !samePeerURL("http://a:1/", "http://a:1") || samePeerURL("http://a:1", "http://a:2") {
		t.Fatal("url compare broken")
	}
}

func TestRemovePeerBlockComments(t *testing.T) {
	dir := t.TempDir()
	orig := `[daemon]
addr = "127.0.0.1:8379"


[peers.wsl] # office box
url = "http://127.0.0.1:9"

[peers.wsl.servers.g] # nested comment
priority = -1

[peers.other]
url = "http://127.0.0.1:8"
`
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePeerBlock(dir, "wsl"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "oxyp.toml"))
	if strings.Contains(string(got), "peers.wsl") || strings.Contains(string(got), "127.0.0.1:9") {
		t.Fatalf("commented block must be gone:\n%s", got)
	}
	// Byte-precise elsewhere: the double blank line above [peers.wsl] stays.
	if !strings.Contains(string(got), "addr = \"127.0.0.1:8379\"\n\n\n[peers.other]") {
		t.Fatalf("unrelated bytes must be untouched:\n%s", got)
	}
}

func TestRemovePeerBlockMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oxyp.toml"), []byte("[daemon]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePeerBlock(dir, "nope"); err == nil {
		t.Fatal("missing peer must error, not silently succeed")
	}
}

func TestIsPeerHeaderComment(t *testing.T) {
	if !isPeerHeader("[peers.wsl] # box", "wsl") {
		t.Fatal("trailing comment must not break header match")
	}
	if !isPeerHeader(`[peers."w#sl"] # x`, "w#sl") {
		t.Fatal("# inside quotes is literal")
	}
	if isPeerHeader("[peers.other] # wsl", "wsl") {
		t.Fatal("comment must not match a different peer")
	}
}
