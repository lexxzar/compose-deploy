package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lexxzar/compose-deploy/internal/compose"
)

const (
	// inspectDefaultWidth stands in when the caller has no terminal width yet
	// (m.width == 0 before the first WindowSizeMsg, and in tests), mirroring the
	// same fallback in formatLogContent.
	inspectDefaultWidth = 80

	// inspectValueCol is the column the value half of a "label   value" row
	// starts at, counted from the left edge of the summary.
	inspectValueCol = 18

	// inspectMinValueWidth is the narrowest value column still worth aligning.
	// Below it the value is stacked under its label instead, so a narrow pane
	// degrades to two readable lines rather than one-rune-per-line shredding.
	inspectMinValueWidth = 12

	// inspectBlockIndent is the left pad of a free-form block (a health probe's
	// output), one level deeper than a value row's label.
	inspectBlockIndent = 4
)

// inspectBuilder accumulates the summary line by line. Every push goes through
// a soft wrap at the target width, so the caller never has to think about the
// never-exceed-width invariant.
type inspectBuilder struct {
	width int
	lines []string
}

// push appends one already-fitted line, dropping trailing padding.
func (b *inspectBuilder) push(line string) {
	b.lines = append(b.lines, strings.TrimRight(line, " "))
}

// section starts a named block, separated from the previous one by a blank line.
func (b *inspectBuilder) section(title string) {
	if len(b.lines) > 0 {
		b.push("")
	}
	b.push(clampToWidth("  "+helpGroupTitleStyle.Render(title), b.width))
}

// kv writes a "label   value" row. The value is soft-wrapped, with continuation
// lines aligned under the value column; it is never truncated.
func (b *inspectBuilder) kv(label, value string) {
	head := "  " + label
	if n := utf8.RuneCountInString(head); n < inspectValueCol {
		head += strings.Repeat(" ", inspectValueCol-n)
	} else {
		head += " "
	}

	indent := utf8.RuneCountInString(head)
	if b.width-indent < inspectMinValueWidth {
		for _, chunk := range softWrapLine(strings.TrimRight(head, " "), b.width) {
			b.push(chunk)
		}
		b.block(inspectBlockIndent, value)
		return
	}

	pad := strings.Repeat(" ", indent)
	first := true
	for _, para := range strings.Split(value, "\n") {
		for _, chunk := range softWrapLine(para, b.width-indent) {
			if first {
				b.push(head + chunk)
				first = false
				continue
			}
			b.push(pad + chunk)
		}
	}
}

// block writes free-form text (a health probe's output) at the given indent.
// Embedded newlines are kept and every resulting line is soft-wrapped — probe
// output is the reason this screen exists, so it must never be cut at the
// terminal edge.
func (b *inspectBuilder) block(indent int, text string) {
	if indent >= b.width {
		indent = 0
	}
	avail := max(b.width-indent, 1)
	pad := strings.Repeat(" ", indent)
	for _, para := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for _, chunk := range softWrapLine(strings.TrimRight(para, " \t"), avail) {
			b.push(pad + chunk)
		}
	}
}

func (b *inspectBuilder) String() string {
	return strings.Join(b.lines, "\n")
}

// buildInspectSummary renders the curated summary of one container's
// `docker inspect` output. Pure — no Model state, no TTY and no Docker — so it
// is golden-testable against the real fixtures in internal/compose/testdata.
//
// A section with nothing to say is omitted; STATE always renders.
func buildInspectSummary(doc compose.InspectDoc, width int) string {
	if width <= 0 {
		width = inspectDefaultWidth
	}
	b := &inspectBuilder{width: width}
	inspectStateSection(b, doc)
	inspectHealthSection(b, doc)
	return b.String()
}

func inspectStateSection(b *inspectBuilder, doc compose.InspectDoc) {
	b.section("STATE")

	status := doc.State.Status
	if status == "" {
		status = "unknown"
	}
	b.kv("status", status)

	// A running container's ExitCode is always 0 and says nothing; on a stopped
	// one it is the answer the user came for.
	if !doc.State.Running {
		b.kv("exit code", strconv.Itoa(doc.State.ExitCode))
	}
	if doc.State.OOMKilled {
		b.kv("oom killed", "yes")
	}
	if started := formatInspectTime(doc.State.StartedAt); started != "" {
		b.kv("started", started)
	}
	b.kv("restart policy", formatRestartPolicy(doc.HostConfig.RestartPolicy))
	b.kv("restarts", strconv.Itoa(doc.RestartCount))
}

func inspectHealthSection(b *inspectBuilder, doc compose.InspectDoc) {
	hc, state := doc.Config.Healthcheck, doc.State.Health
	if !hasHealthcheck(hc, state) {
		return
	}
	b.section("HEALTH")

	if state != nil {
		status := state.Status
		if status == "" {
			status = "unknown"
		}
		b.kv("status", status)
		b.kv("failing streak", strconv.Itoa(state.FailingStreak))
	}
	if hc != nil {
		if test := strings.Join(hc.Test, " "); test != "" {
			b.kv("test", test)
		}
		if hc.Interval > 0 {
			b.kv("interval", hc.Interval.String())
		}
		if hc.Timeout > 0 {
			b.kv("timeout", hc.Timeout.String())
		}
		if hc.Retries > 0 {
			b.kv("retries", strconv.Itoa(hc.Retries))
		}
	}

	probe, ok := lastHealthProbe(state)
	if !ok {
		return
	}
	b.kv("last probe", formatProbeHeader(probe))
	if out := strings.TrimRight(probe.Output, "\n"); out != "" {
		b.block(inspectBlockIndent, out)
	}
}

// hasHealthcheck reports whether the container has a healthcheck to describe.
// Docker omits both keys for an image without one, and reports Test ["NONE"]
// with no runtime state when the healthcheck is explicitly disabled — both mean
// the HEALTH section would say nothing, so it is dropped.
func hasHealthcheck(hc *compose.InspectHealthcheck, state *compose.InspectHealth) bool {
	if state != nil {
		return true
	}
	if hc == nil {
		return false
	}
	return !(len(hc.Test) == 1 && hc.Test[0] == "NONE")
}

// lastHealthProbe returns the most recent probe result. Docker appends to the
// Log, so the last element is the newest.
func lastHealthProbe(state *compose.InspectHealth) (compose.InspectHealthLog, bool) {
	if state == nil || len(state.Log) == 0 {
		return compose.InspectHealthLog{}, false
	}
	return state.Log[len(state.Log)-1], true
}

func formatProbeHeader(p compose.InspectHealthLog) string {
	head := fmt.Sprintf("exit %d", p.ExitCode)
	if end := formatInspectTime(p.End); end != "" {
		head += " at " + end
	}
	return head
}

func formatRestartPolicy(p compose.InspectRestartPolicy) string {
	if p.Name == "" {
		return "no"
	}
	if p.MaximumRetryCount > 0 {
		return fmt.Sprintf("%s (max %d)", p.Name, p.MaximumRetryCount)
	}
	return p.Name
}

// formatInspectTime renders one of docker's RFC3339 timestamps. The zone of the
// timestamp is kept rather than converted, matching parseCreatedAt and the
// Created column. Docker writes the zero time for a container that never ran,
// which yields "" so the caller omits the row; an unparseable value is passed
// through so nothing is silently lost.
func formatInspectTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	if t.Year() <= 1 {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
