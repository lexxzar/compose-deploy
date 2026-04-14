package tui

import (
	"encoding/json"
	"strings"
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
		pad := strings.Repeat(" ", len(prefix)+3) // 3 for " | "
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

// softWrapLine breaks a single line into chunks of at most width characters.
func softWrapLine(line string, width int) []string {
	if width <= 0 || len(line) <= width {
		return []string{line}
	}

	var result []string
	for len(line) > width {
		result = append(result, line[:width])
		line = line[width:]
	}
	result = append(result, line)
	return result
}
