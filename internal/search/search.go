// Package search ranks tools by words, no model. Space-separated query
// words match case-insensitively against tool name, server name +
// description, and tool description; weights: name 3, server 2,
// description 1. Regex mode matches unranked. Deny-filtered tools never
// reach the ranker (callers filter first).
package search

import (
	"regexp"
	"sort"
	"strings"
)

// Candidate is one searchable tool.
type Candidate struct {
	Server            string
	ServerDescription string
	Name              string
	Description       string
}

// Result is a ranked candidate.
type Result struct {
	Candidate
	Score int
}

// Rank scores candidates against space-separated words, best first.
// Zero-score candidates are dropped.
func Rank(query string, cands []Candidate) []Result {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}
	var out []Result
	for _, c := range cands {
		name := strings.ToLower(c.Name)
		srv := strings.ToLower(c.Server + " " + c.ServerDescription)
		desc := strings.ToLower(c.Description)
		score := 0
		for _, w := range words {
			switch {
			case strings.Contains(name, w):
				score += 3
			case strings.Contains(srv, w):
				score += 2
			case strings.Contains(desc, w):
				score++
			}
		}
		if score > 0 {
			out = append(out, Result{Candidate: c, Score: score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Regex filters candidates by pattern against "server/name description".
// Unranked (server, name order). Invalid pattern returns the error.
func Regex(pattern string, cands []Candidate) ([]Result, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, c := range cands {
		if re.MatchString(c.Server + "/" + c.Name + " " + c.Description) {
			out = append(out, Result{Candidate: c})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Page slices results with limit/offset. limit <= 0 = default 12.
func Page(res []Result, limit, offset int) (page []Result, nextOffset int) {
	if limit <= 0 {
		limit = 12
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(res) {
		return nil, len(res)
	}
	end := min(offset+limit, len(res))
	next := end
	if next >= len(res) {
		next = -1 // no more pages
	}
	return res[offset:end], next
}
