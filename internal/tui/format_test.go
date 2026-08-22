package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFormatLogContent_Empty(t *testing.T) {
	got := formatLogContent("", 80, true, true)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestFormatLogContent_PlainTextPassthrough(t *testing.T) {
	raw := "app | starting server on port 8080"
	got := formatLogContent(raw, 80, false, false)
	if got != raw {
		t.Errorf("got %q, want %q", got, raw)
	}
}

func TestFormatLogContent_PrettyJSON(t *testing.T) {
	raw := `app | {"level":"info","msg":"hello"}`
	got := formatLogContent(raw, 200, false, true)

	if !strings.Contains(got, "app | {") {
		t.Errorf("first line should have prefix, got:\n%s", got)
	}
	if !strings.Contains(got, `  "level": "info"`) {
		t.Errorf("should contain indented level field, got:\n%s", got)
	}
	if !strings.Contains(got, `  "msg": "hello"`) {
		t.Errorf("should contain indented msg field, got:\n%s", got)
	}

	// Continuation lines should be padded to align with body
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected multiple lines, got %d", len(lines))
	}
	// "app" is 3 chars, " | " is 3, so pad = 6 spaces
	expectedPad := "      "
	for i := 1; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], expectedPad) {
			t.Errorf("line %d should start with %d spaces padding, got %q", i, len(expectedPad), lines[i])
		}
	}
}

func TestFormatLogContent_PrettyNestedJSON(t *testing.T) {
	raw := `svc | {"a":{"b":"c"}}`
	got := formatLogContent(raw, 200, false, true)

	if !strings.Contains(got, `"a": {`) {
		t.Errorf("should contain nested object key, got:\n%s", got)
	}
	if !strings.Contains(got, `"b": "c"`) {
		t.Errorf("should contain nested field, got:\n%s", got)
	}
}

func TestFormatLogContent_PrettyNonJSON(t *testing.T) {
	raw := "app | not json at all"
	got := formatLogContent(raw, 80, false, true)
	if got != raw {
		t.Errorf("non-JSON should pass through unchanged, got %q", got)
	}
}

func TestFormatLogContent_PrettyMixedContent(t *testing.T) {
	raw := "app | plain text\napp | {\"key\":\"value\"}\napp | more plain"
	got := formatLogContent(raw, 200, false, true)

	lines := strings.Split(got, "\n")
	if lines[0] != "app | plain text" {
		t.Errorf("line 0 should be plain text, got %q", lines[0])
	}
	if !strings.Contains(got, `"key": "value"`) {
		t.Errorf("JSON line should be pretty-printed, got:\n%s", got)
	}
	// Last line should be the plain text
	if lines[len(lines)-1] != "app | more plain" {
		t.Errorf("last line should be plain text, got %q", lines[len(lines)-1])
	}
}

func TestFormatLogContent_PrettyEmptyJSON(t *testing.T) {
	raw := "app | {}"
	got := formatLogContent(raw, 80, false, true)
	if got != "app | {}" {
		t.Errorf("empty JSON object should stay on one line, got %q", got)
	}
}

func TestFormatLogContent_PrettyNoPrefix(t *testing.T) {
	raw := `{"level":"info","msg":"standalone"}`
	got := formatLogContent(raw, 200, false, true)

	if !strings.Contains(got, `"level": "info"`) {
		t.Errorf("prefix-less JSON should still be pretty-printed, got:\n%s", got)
	}
}

func TestFormatLogContent_SoftWrapLong(t *testing.T) {
	raw := strings.Repeat("x", 30)
	got := formatLogContent(raw, 10, true, false)

	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines for 30-char line at width 10, got %d: %v", len(lines), lines)
	}
	for i, l := range lines {
		if len(l) > 10 {
			t.Errorf("line %d length %d exceeds width 10: %q", i, len(l), l)
		}
	}
}

func TestFormatLogContent_SoftWrapShort(t *testing.T) {
	raw := "short"
	got := formatLogContent(raw, 80, true, false)
	if got != "short" {
		t.Errorf("short line should not be wrapped, got %q", got)
	}
}

func TestFormatLogContent_SoftWrapExactWidth(t *testing.T) {
	raw := strings.Repeat("x", 10)
	got := formatLogContent(raw, 10, true, false)
	if got != raw {
		t.Errorf("exact-width line should not wrap, got %q", got)
	}
}

func TestFormatLogContent_WrapAndPretty(t *testing.T) {
	raw := `app | {"longkey":"` + strings.Repeat("v", 60) + `"}`
	got := formatLogContent(raw, 40, true, true)

	lines := strings.Split(got, "\n")
	for i, l := range lines {
		if len(l) > 40 {
			t.Errorf("line %d length %d exceeds width 40: %q", i, len(l), l)
		}
	}
	// Should still contain the JSON content somewhere
	full := strings.Join(lines, "")
	if !strings.Contains(full, "longkey") {
		t.Errorf("wrapped pretty content should contain key, got:\n%s", got)
	}
}

func TestFormatLogContent_MultilineRaw(t *testing.T) {
	raw := "line1\nline2\nline3"
	got := formatLogContent(raw, 80, false, false)
	if got != raw {
		t.Errorf("raw passthrough should preserve all lines, got %q", got)
	}
}

func TestFormatLogContent_WrapMultipleLines(t *testing.T) {
	raw := strings.Repeat("a", 15) + "\n" + strings.Repeat("b", 5)
	got := formatLogContent(raw, 10, true, false)

	lines := strings.Split(got, "\n")
	// First line splits into 2 (15 chars at width 10), second stays 1
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
}

func TestSplitLogPrefix(t *testing.T) {
	tests := []struct {
		line       string
		wantPrefix string
		wantBody   string
		wantOK     bool
	}{
		{"app | hello", "app", "hello", true},
		{"my-service | {}", "my-service", "{}", true},
		{"no separator here", "", "", false},
		{" | empty prefix", "", "empty prefix", true},
		{"app | one | two", "app", "one | two", true}, // splits on first only
	}

	for _, tt := range tests {
		prefix, body, ok := splitLogPrefix(tt.line)
		if ok != tt.wantOK || prefix != tt.wantPrefix || body != tt.wantBody {
			t.Errorf("splitLogPrefix(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.line, prefix, body, ok, tt.wantPrefix, tt.wantBody, tt.wantOK)
		}
	}
}

func TestSoftWrapLine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		width int
		want  int // expected number of resulting lines
	}{
		{"empty", "", 10, 1},
		{"short", "hello", 10, 1},
		{"exact", "1234567890", 10, 1},
		{"one over", "12345678901", 10, 2},
		{"double", strings.Repeat("x", 20), 10, 2},
		{"triple", strings.Repeat("x", 30), 10, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := softWrapLine(tt.line, tt.width)
			if len(got) != tt.want {
				t.Errorf("softWrapLine(%q, %d) produced %d lines, want %d", tt.line, tt.width, len(got), tt.want)
			}
		})
	}
}

func TestSoftWrapLine_UTF8(t *testing.T) {
	// 6 runes: "日本語テスト" — each is 3 bytes (18 bytes total)
	line := "日本語テスト"
	got := softWrapLine(line, 4)

	// Should split at 4 runes, not 4 bytes
	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(got), got)
	}
	if got[0] != "日本語テ" {
		t.Errorf("first chunk = %q, want %q", got[0], "日本語テ")
	}
	if got[1] != "スト" {
		t.Errorf("second chunk = %q, want %q", got[1], "スト")
	}
}

func TestSoftWrapLine_MixedASCIIUTF8(t *testing.T) {
	line := "ab日cd" // 5 runes, 7 bytes
	got := softWrapLine(line, 3)

	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(got), got)
	}
	if got[0] != "ab日" {
		t.Errorf("first chunk = %q, want %q", got[0], "ab日")
	}
	if got[1] != "cd" {
		t.Errorf("second chunk = %q, want %q", got[1], "cd")
	}
}

func TestFormatLogLines(t *testing.T) {
	lines := []string{
		`app | {"level":"info"}`,
		"app | plain text",
	}
	got := formatLogLines(lines, 200, false, true)
	if !strings.Contains(got, `"level": "info"`) {
		t.Errorf("should pretty-print JSON line, got:\n%s", got)
	}
	if !strings.Contains(got, "app | plain text") {
		t.Errorf("should pass through plain text, got:\n%s", got)
	}
}

func TestTryPrettyJSON(t *testing.T) {
	// Valid JSON
	lines, ok := tryPrettyJSON(`{"a":"b"}`)
	if !ok {
		t.Error("should detect valid JSON")
	}
	if len(lines) < 3 { // {, field, }
		t.Errorf("expected at least 3 lines, got %d", len(lines))
	}

	// Invalid JSON
	_, ok = tryPrettyJSON("not json")
	if ok {
		t.Error("should not detect non-JSON")
	}

	// Empty string
	_, ok = tryPrettyJSON("")
	if ok {
		t.Error("should not detect empty string as JSON")
	}

	// Whitespace only
	_, ok = tryPrettyJSON("   ")
	if ok {
		t.Error("should not detect whitespace as JSON")
	}
}

// TestExpandTabs pins the tab-stop expansion the width invariant depends on.
// ansi.StringWidth counts a tab as zero cells, so a value carrying one measures
// narrow, wraps late and renders past the pane edge.
func TestExpandTabs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no tab", "plain value", "plain value"},
		{"leading tab", "\tx", "        x"},
		{"one column in", "a\tb", "a       b"},
		{"exactly at a stop", "12345678\tx", "12345678        x"},
		{"one before a stop", "1234567\tx", "1234567 x"},
		{"consecutive", "a\t\tb", "a               b"},
		{"stack trace", "\tmain.run()\n", "        main.run()\n"},
		{"wide runes count cells", "あ\tx", "あ      x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandTabs(tc.in); got != tc.want {
				t.Errorf("expandTabs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestWrapCells pins the difference from softWrapLine that the inspect
// summary's never-exceed-width invariant rests on: a wide grapheme is 2 cells,
// so a rune-counting wrap hands back chunks that render twice the pane width.
func TestWrapCells(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"fits", "abc", 10, []string{"abc"}},
		{"zero width is unbounded", "abcdef", 0, []string{"abcdef"}},
		{"ascii splits at the width", "abcdef", 3, []string{"abc", "def"}},
		{"wide runes split by cell", "あいうえ", 4, []string{"あい", "うえ"}},
		{"tabs are expanded first", "a\tb", 4, []string{"a   ", "    ", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapCells(tc.in, tc.width)
			if strings.Join(got, "") != expandTabs(tc.in) {
				t.Errorf("wrapCells(%q, %d) = %q, chunks must concatenate back to the input", tc.in, tc.width, got)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("wrapCells(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("wrapCells(%q, %d)[%d] = %q, want %q", tc.in, tc.width, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestWrapCells_BeatsSoftWrapLineOnWideRunes is the regression the cell-aware
// wrap exists for: softWrapLine counts runes, so it declares a CJK line fitted
// at a width it renders at 2x.
func TestWrapCells_BeatsSoftWrapLineOnWideRunes(t *testing.T) {
	const line = "あいうえおかきくけこ"
	const width = 6

	for i, chunk := range wrapCells(line, width) {
		if w := ansi.StringWidth(chunk); w > width {
			t.Errorf("wrapCells chunk %d is %d cells, want <= %d: %q", i, w, width, chunk)
		}
	}
	over := false
	for _, chunk := range softWrapLine(line, width) {
		if ansi.StringWidth(chunk) > width {
			over = true
		}
	}
	if !over {
		t.Error("softWrapLine no longer overruns on wide runes; wrapCells has lost its reason to exist")
	}
}
