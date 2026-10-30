package search

import (
	"testing"
)

func cands() []Candidate {
	return []Candidate{
		{Server: "github", Name: "search_code", Description: "search code in repos"},
		{Server: "github", Name: "get_file", Description: "read a file"},
		{Server: "notes", Name: "grep_notes", Description: "search personal notes"},
	}
}

func TestRankNameBeatsDescription(t *testing.T) {
	res := Rank("grep", cands())
	if len(res) != 1 || res[0].Name != "grep_notes" {
		t.Fatalf("got %+v", res)
	}
	// "search" hits name (search_code) + descriptions
	res = Rank("search", cands())
	if len(res) != 2 || res[0].Name != "search_code" {
		t.Fatalf("name should win: %+v", res)
	}
}

func TestRankServerMatches(t *testing.T) {
	res := Rank("github", cands())
	if len(res) != 2 {
		t.Fatalf("want both github tools, got %+v", res)
	}
}

func TestRankMultiWord(t *testing.T) {
	res := Rank("github file", cands())
	if len(res) == 0 || res[0].Name != "get_file" {
		t.Fatalf("multi-word should prefer get_file: %+v", res)
	}
}

func TestRankEmpty(t *testing.T) {
	if Rank("", cands()) != nil || Rank("zzz-no-match", cands()) != nil {
		t.Fatal("empty/no-match must return nil")
	}
}

func TestRegex(t *testing.T) {
	res, err := Regex("github/.*file", cands())
	if err != nil || len(res) != 1 || res[0].Name != "get_file" {
		t.Fatalf("got %+v err=%v", res, err)
	}
	if _, err := Regex("([", cands()); err == nil {
		t.Fatal("bad pattern must error")
	}
}

func TestPage(t *testing.T) {
	var res []Result
	for range 5 {
		res = append(res, Result{})
	}
	p, next := Page(res, 2, 0)
	if len(p) != 2 || next != 2 {
		t.Fatalf("p=%d next=%d", len(p), next)
	}
	p, next = Page(res, 2, 4)
	if len(p) != 1 || next != -1 {
		t.Fatalf("last page: p=%d next=%d", len(p), next)
	}
	p, next = Page(res, 2, 9)
	if len(p) != 0 || next != 5 {
		t.Fatalf("past end: p=%d next=%d", len(p), next)
	}
}
