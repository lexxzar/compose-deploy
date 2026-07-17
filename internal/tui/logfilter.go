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

// foldNewRawLines folds the not-yet-processed tail of the raw buffer
// (raw[scanned:]) through the filter predicate and the wrap/pretty formatter,
// returning the formatted delta to append to the cached content and the new
// scan cursor.
//
// The cursor ALWAYS advances to len(raw): it counts raw lines scanned, not
// survivors folded. This is the survivor-cursor trap — if L0(pass), L1(fail),
// L2(pass) arrive, advancing by survivor count (2) instead of raw count (3)
// would re-scan the already-filtered L1 on the next call and duplicate output.
// A nil pred (no active filter) passes every line.
func foldNewRawLines(raw []string, scanned, width int, wrap, pretty bool, pred func(string) bool) (delta string, newScanned int) {
	if scanned >= len(raw) {
		return "", scanned
	}
	survivors := deriveFiltered(raw[scanned:], pred)
	if len(survivors) == 0 {
		return "", len(raw)
	}
	return formatLogLines(survivors, width, wrap, pretty), len(raw)
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

// highlightMatches overlays the search highlight on the rendered physical log
// lines. Each matched line (its index present in matches) is wrapped in
// logSearchMatchStyle; the current match's line (index cur) is wrapped in
// logSearchCurrentStyle (bold) so it stands out among the matches.
//
// The WHOLE physical line is wrapped rather than a matched sub-span because the
// caller passes only line indices, not the query — there is no substring offset
// to locate. Wrapping the line still leaves ansi.StringWidth unaffected either
// way, since a foreground-only style adds only zero-width ANSI escapes (same
// rationale as the container-search name highlight). An empty matches slice
// returns physical unchanged; cur == -1 (no current match) skips the bold pass.
func highlightMatches(physical []string, matches []int, cur int) []string {
	if len(matches) == 0 {
		return physical
	}
	matchSet := make(map[int]struct{}, len(matches))
	for _, idx := range matches {
		matchSet[idx] = struct{}{}
	}
	out := make([]string, len(physical))
	for i, line := range physical {
		if i == cur {
			out[i] = logSearchCurrentStyle.Render(line)
			continue
		}
		if _, ok := matchSet[i]; ok {
			out[i] = logSearchMatchStyle.Render(line)
			continue
		}
		out[i] = line
	}
	return out
}
