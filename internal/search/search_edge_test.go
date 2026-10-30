package search

import (
	"strings"
	"testing"
)

func TestRankScoreTable(t *testing.T) {
	c := Candidate{Server: "srv", ServerDescription: "srvdesc", Name: "mytool", Description: "does things"}
	cases := []struct {
		name  string
		query string
		cands []Candidate
		want  int // expected score of first result; -1 = expect nil
	}{
		{"name hit", "mytool", []Candidate{c}, 3},
		{"server hit", "srv", []Candidate{c}, 2},
		{"serverdesc hit", "srvdesc", []Candidate{c}, 2},
		{"desc hit", "things", []Candidate{c}, 1},
		// name-wins exclusivity: one word matching name AND desc scores 3, not 3+1.
		{"name wins exclusive", "mytool", []Candidate{{
			Server: "s", Name: "mytool", Description: "mytool inside desc",
		}}, 3},
		// repeated words count repeatedly (current behavior).
		{"repeated word doubles", "mytool mytool", []Candidate{c}, 6},
		// multi-word sums across fields.
		{"two words", "mytool things", []Candidate{c}, 4},
		{"no match", "zzz", []Candidate{c}, -1},
		{"empty query", "", []Candidate{c}, -1},
		{"whitespace query", "   ", []Candidate{c}, -1},
	}
	for _, tc := range cases {
		res := Rank(tc.query, tc.cands)
		if tc.want < 0 {
			if res != nil {
				t.Errorf("%s: Rank(%q) = %+v, want nil", tc.name, tc.query, res)
			}
			continue
		}
		if len(res) != 1 || res[0].Score != tc.want {
			t.Errorf("%s: Rank(%q) score = %+v, want %d", tc.name, tc.query, res, tc.want)
		}
	}
}

func TestRankNameWinsExclusivity(t *testing.T) {
	// Same word in name and description must not stack.
	a := Candidate{Server: "s", Name: "alpha", Description: "alpha beta"}
	res := Rank("alpha", []Candidate{a})
	if len(res) != 1 || res[0].Score != 3 {
		t.Fatalf("exclusive name score: %+v", res)
	}
	// Word in server and description (not name): server wins, no stack.
	b := Candidate{Server: "gamma", Name: "tool", Description: "gamma stuff"}
	res = Rank("gamma", []Candidate{b})
	if len(res) != 1 || res[0].Score != 2 {
		t.Fatalf("exclusive server score: %+v", res)
	}
}

func TestRankRepeatedWords(t *testing.T) {
	// current behavior: repeated query words score repeatedly.
	c := Candidate{Server: "s", Name: "tool", Description: "w"}
	res := Rank("w w w", []Candidate{c})
	if len(res) != 1 || res[0].Score != 3 {
		t.Fatalf("repeated desc word: %+v (current behavior: counted per occurrence)", res)
	}
}

func TestRankTieOrder(t *testing.T) {
	cands := []Candidate{
		{Server: "b", Name: "zeta", Description: "hit"},
		{Server: "a", Name: "zeta", Description: "hit"},
		{Server: "a", Name: "alpha", Description: "hit"},
	}
	res := Rank("hit", cands)
	if len(res) != 3 {
		t.Fatalf("got %+v", res)
	}
	// all score 1; order must be (a,alpha),(a,zeta),(b,zeta).
	if res[0].Name != "alpha" || res[0].Server != "a" ||
		res[1].Name != "zeta" || res[1].Server != "a" ||
		res[2].Server != "b" {
		t.Fatalf("tie order wrong: %+v", res)
	}
}

func TestRankCaseAndSubstring(t *testing.T) {
	c := []Candidate{{Server: "GitHub", Name: "Search_Code", Description: "Find Stuff"}}
	for _, q := range []string{"github", "GITHUB", "GitHub", "search", "SEARCH", "cod", "COD", "stu"} {
		if len(Rank(q, c)) != 1 {
			t.Errorf("Rank(%q) should match (case-insensitive substring)", q)
		}
	}
	if Rank("zzz", c) != nil {
		t.Error("non-substring must not match")
	}
}

func TestRankServerDescriptionCounts(t *testing.T) {
	// ServerDescription is folded into the server field (weight 2).
	c := []Candidate{{Server: "s", ServerDescription: "postgreSQL database", Name: "t", Description: "d"}}
	res := Rank("postgresql", c)
	if len(res) != 1 || res[0].Score != 2 {
		t.Fatalf("server-description hit should score 2: %+v", res)
	}
}

func TestRegexSemantics(t *testing.T) {
	cands := []Candidate{
		{Server: "github", ServerDescription: "code hosting", Name: "get_file", Description: "read a file"},
		{Server: "notes", ServerDescription: "personal", Name: "grep_notes", Description: "search notes"},
	}
	// ServerDescription is EXCLUDED from Regex haystack (current behavior).
	res, err := Regex("code hosting", cands)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("Regex must not match ServerDescription (current behavior): %+v", res)
	}
	// Empty pattern matches everything (Go regexp "" matches all).
	res, err = Regex("", cands)
	if err != nil || len(res) != 2 {
		t.Fatalf("empty pattern should match all: %+v err=%v", res, err)
	}
	// Score is always 0 in Regex mode.
	for _, r := range res {
		if r.Score != 0 {
			t.Fatalf("Regex result score must be 0: %+v", r)
		}
	}
	// Case-sensitive (unlike Rank).
	res, err = Regex("GITHUB", cands)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("Regex is case-sensitive: %+v", res)
	}
	// Description part of haystack.
	res, err = Regex("read a file", cands)
	if err != nil || len(res) != 1 || res[0].Name != "get_file" {
		t.Fatalf("desc match: %+v err=%v", res, err)
	}
	// Invalid pattern errors, nil results.
	if res, err := Regex("([", cands); err == nil || res != nil {
		t.Fatalf("bad pattern: res=%v err=%v", res, err)
	}
}

func TestRegexOrderServerName(t *testing.T) {
	cands := []Candidate{
		{Server: "b", Name: "a_tool", Description: "x"},
		{Server: "a", Name: "z_tool", Description: "x"},
		{Server: "a", Name: "a_tool", Description: "x"},
	}
	res, err := Regex("tool", cands)
	if err != nil || len(res) != 3 {
		t.Fatalf("got %+v err=%v", res, err)
	}
	if res[0].Server != "a" || res[0].Name != "a_tool" ||
		res[1].Name != "z_tool" || res[2].Server != "b" {
		t.Fatalf("regex order must be server,name: %+v", res)
	}
}

func TestPageContract(t *testing.T) {
	mk := func(n int) []Result {
		var r []Result
		for i := range n {
			r = append(r, Result{Candidate: Candidate{Name: string(rune('a' + i))}, Score: i})
		}
		return r
	}
	res := mk(5)
	cases := []struct {
		name      string
		limit     int
		offset    int
		wantLen   int
		wantNext  int
		wantFirst string // name of first element; "" if empty page
	}{
		{"first page", 2, 0, 2, 2, "a"},
		{"last partial → terminal -1", 2, 4, 1, -1, "e"},
		{"exact fit → terminal -1", 5, 0, 5, -1, "a"},
		{"past end → nil + len", 2, 9, 0, 5, ""},
		{"offset == len → nil + len", 2, 5, 0, 5, ""},
		{"limit 0 → default 12 (all)", 0, 0, 5, -1, "a"},
		{"limit negative → default 12 (all)", -3, 0, 5, -1, "a"},
		{"offset -1 → clamped to 0", 2, -1, 2, 2, "a"},
		{"empty input", 2, 0, 0, 0, ""}, // offset 0 >= len 0 → nil, next=len(res)=0 (past-end contract)
	}
	for _, tc := range cases {
		in := res
		if tc.name == "empty input" {
			in = nil
		}
		p, next := Page(in, tc.limit, tc.offset)
		if len(p) != tc.wantLen || next != tc.wantNext {
			t.Errorf("%s: Page(len=%d,limit=%d,offset=%d) = len %d,next %d; want len %d,next %d",
				tc.name, len(in), tc.limit, tc.offset, len(p), next, tc.wantLen, tc.wantNext)
		}
		if tc.wantLen > 0 && p[0].Name != tc.wantFirst {
			t.Errorf("%s: first = %q, want %q", tc.name, p[0].Name, tc.wantFirst)
		}
	}
}

func TestPageTerminalMinusOne(t *testing.T) {
	// A full non-final page returns the next offset; the page that
	// reaches the end returns -1.
	var res []Result
	for range 4 {
		res = append(res, Result{})
	}
	if _, next := Page(res, 3, 0); next != 3 {
		t.Fatalf("non-final page next = %d, want 3", next)
	}
	if _, next := Page(res, 3, 3); next != -1 {
		t.Fatalf("final page next = %d, want -1", next)
	}
}

func TestPageHugeLimitNoPanic(t *testing.T) {
	var res []Result
	for range 5 {
		res = append(res, Result{})
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Page with huge limit panicked: %v", r)
		}
	}()
	p, next := Page(res, 1<<30, 0)
	if len(p) != 5 || next != -1 {
		t.Fatalf("huge limit: len=%d next=%d", len(p), next)
	}
	p, next = Page(res, 1<<30, 4)
	if len(p) != 1 || next != -1 {
		t.Fatalf("huge limit at tail: len=%d next=%d", len(p), next)
	}
}

func TestPageNoMutate(t *testing.T) {
	res := []Result{
		{Candidate: Candidate{Name: "a"}, Score: 1},
		{Candidate: Candidate{Name: "b"}, Score: 2},
		{Candidate: Candidate{Name: "c"}, Score: 3},
	}
	snapshot := append([]Result(nil), res...)
	p, _ := Page(res, 2, 0)
	if len(res) != len(snapshot) {
		t.Fatal("Page changed input length")
	}
	for i := range res {
		if res[i] != snapshot[i] {
			t.Fatalf("Page mutated input at %d", i)
		}
	}
	// Returned page shares the backing array (subslice); document it.
	p[0].Name = "MUT"
	if res[0].Name != "MUT" {
		t.Log("current behavior: page does not alias input (copy)")
	} else {
		t.Log("current behavior: page aliases input backing array")
	}
	res[0].Name = "a" // restore
	_ = p
}

func TestRankDoesNotMutateInput(t *testing.T) {
	in := []Candidate{
		{Server: "b", Name: "z", Description: "hit"},
		{Server: "a", Name: "a", Description: "hit"},
	}
	orig0, orig1 := in[0], in[1]
	_ = Rank("hit", in)
	if in[0] != orig0 || in[1] != orig1 {
		t.Fatal("Rank mutated input order")
	}
	_ = strings.Contains("x", "x") // keep strings import if unused elsewhere
}
