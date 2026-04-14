package tui

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// formatLogContent applies pretty-print and/or soft-wrap transformations to raw log content.
// Processing order: pretty-print first (expands JSON), then soft-wrap.
func formatLogContent(raw string, width int, wrap bool, pretty bool) string {
	if raw == "" {
		return ""
	}
	if width <= 0 {
		width = 80
	}

	lines := strings.Split(raw, "\n")

	if pretty {
		var out []string
		for _, line := range lines {
			out = append(out, prettyPrintLine(line)...)
		}
		lines = out
	}

	if wrap {
		var out []string
		for _, line := range lines {
			out = append(out, softWrapLine(line, width)...)
		}
		lines = out
	}

	return strings.Join(lines, "\n")
}

// formatLogLines formats a slice of raw log lines through pretty-print and/or soft-wrap.
// Used for incremental formatting of new lines without reprocessing the entire content.
func formatLogLines(lines []string, width int, wrap bool, pretty bool) string {
	if len(lines) == 0 {
		return ""
	}
	if width <= 0 {
		width = 80
	}

	result := lines
	if pretty {
		var out []string
		for _, line := range result {
			out = append(out, prettyPrintLine(line)...)
		}
		result = out
	}

	if wrap {
		var out []string
		for _, line := range result {
			out = append(out, softWrapLine(line, width)...)
		}
		result = out
	}

	return strings.Join(result, "\n")
}

// prettyPrintLine attempts to pretty-print JSON in a docker compose log line.
// Docker compose format: "<service> | <body>". If body is valid JSON, it is
// indented with continuation lines padded to align under the body start.
func prettyPrintLine(line string) []string {
	prefix, body, ok := splitLogPrefix(line)
	if !ok {
		// No prefix — try the whole line as JSON
		if expanded, didExpand := tryPrettyJSON(line); didExpand {
			return expanded
		}
		return []string{line}
	}

	if expanded, didExpand := tryPrettyJSON(body); didExpand {
		pad := strings.Repeat(" ", utf8.RuneCountInString(prefix)+3) // 3 for " | "
		result := make([]string, len(expanded))
		result[0] = prefix + " | " + expanded[0]
		for i := 1; i < len(expanded); i++ {
			result[i] = pad + expanded[i]
		}
		return result
	}

	return []string{line}
}

// splitLogPrefix splits a docker compose log line on the first " | ".
// Returns (prefix, body, true) or ("", "", false) if no separator found.
func splitLogPrefix(line string) (string, string, bool) {
	idx := strings.Index(line, " | ")
	if idx < 0 {
		return "", "", false
	}
	return line[:idx], line[idx+3:], true
}

// tryPrettyJSON checks if s is valid JSON and returns indented lines if so.
func tryPrettyJSON(s string) ([]string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, false
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return nil, false
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, false
	}
	return strings.Split(string(pretty), "\n"), true
}

// softWrapLine breaks a single line into chunks of at most width runes.
// Uses rune-aware splitting to avoid corrupting UTF-8 sequences.
func softWrapLine(line string, width int) []string {
	if width <= 0 || utf8.RuneCountInString(line) <= width {
		return []string{line}
	}

	var result []string
	for utf8.RuneCountInString(line) > width {
		// Find byte offset of the width-th rune
		byteOff := 0
		for i := 0; i < width; i++ {
			_, size := utf8.DecodeRuneInString(line[byteOff:])
			byteOff += size
		}
		result = append(result, line[:byteOff])
		line = line[byteOff:]
	}
	result = append(result, line)
	return result
}
