package tui

import (
	"regexp"
	"strings"
)

// buildMatcher compiles a log filter/search query into a reusable predicate.
//
// By default matching is a case-insensitive substring test. When isRegex is
// set the query is compiled once as a Go RE2 pattern (case-sensitive; the user
// controls case via an inline (?i) flag) and reused across every line — the
// regexp is never recompiled per line. When allowNegate is set (filter only,
// where RE2's lack of negative lookahead makes a leading "!" the exclusion
// mechanism), a leading "!" is stripped and the predicate is inverted.
//
// It returns valid=false (with pred=nil and re=nil) when the query is empty
// (including a lone "!" once stripped) or the regex fails to compile, so the
// caller can distinguish "no filter" from a usable predicate and keep its
// last-good matcher on a mid-type compile error. The returned re is non-nil
// only for a valid regex query (even when negated); substring matchers leave
// it nil.
func buildMatcher(query string, isRegex, allowNegate bool) (pred func(string) bool, re *regexp.Regexp, valid bool) {
	if query == "" {
		return nil, nil, false
	}

	negate := false
	if allowNegate && strings.HasPrefix(query, "!") {
		negate = true
		query = query[1:]
	}

	// A lone "!" (or an otherwise empty query after stripping) is not a filter.
	if query == "" {
		return nil, nil, false
	}

	var base func(string) bool
	if isRegex {
		compiled, err := regexp.Compile(query)
		if err != nil {
			return nil, nil, false
		}
		re = compiled
		base = func(s string) bool { return re.MatchString(s) }
	} else {
		q := strings.ToLower(query)
		base = func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	}

	if negate {
		pred = func(s string) bool { return !base(s) }
	} else {
		pred = base
	}
	return pred, re, true
}

// deriveFiltered returns the subset of raw lines for which pred returns true,
// preserving order. A nil pred (no active filter) returns raw unchanged, so the
// derivation pipeline degrades to a pass-through with zero copying.
func deriveFiltered(raw []string, pred func(string) bool) []string {
	if pred == nil {
		return raw
	}
	var out []string
	for _, line := range raw {
		if pred(line) {
			out = append(out, line)
		}
	}
	return out
}

// logComputeMatches returns the ascending indices of physical lines matched by
// pred. It mirrors computeMatches (the container-screen search helper) but
// operates over rendered physical lines and is predicate-driven rather than
// name-substring only. A nil pred (empty query or bad regex) returns nil so
// callers can treat "no query" and "no matches" distinctly.
func logComputeMatches(physical []string, pred func(string) bool) []int {
	if pred == nil {
		return nil
	}
	var matches []int
	for i, line := range physical {
		if pred(line) {
			matches = append(matches, i)
		}
	}
	return matches
}
