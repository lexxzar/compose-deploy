package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
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

	// inspectListIndent is the left pad of a plain list entry (an ENV line),
	// level with the label of a value row rather than one step deeper — the
	// entry is the whole row, not the value half of one.
	inspectListIndent = 2

	// inspectTabWidth is the tab stop tabs are expanded to before a value is
	// measured. See expandTabs.
	inspectTabWidth = 8
)

// expandTabs replaces every tab with spaces up to the next tab stop.
// ansi.StringWidth counts a tab as ZERO cells while a terminal advances the
// cursor to the next multiple of inspectTabWidth, so a tab-bearing value (a Go
// or Java stack trace in a health probe's output, any env value carrying one)
// measures narrow, wraps late and renders wider than the pane — which pushes
// viewInspect past m.height and scrolls the title off. The substituted spaces
// ARE what the terminal draws, so after this the measurement and the render
// agree.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	parts := strings.Split(s, "\t")
	var out strings.Builder
	col := 0
	for i, part := range parts {
		out.WriteString(part)
		col += ansi.StringWidth(part)
		if i == len(parts)-1 {
			break
		}
		pad := inspectTabWidth - col%inspectTabWidth
		out.WriteString(strings.Repeat(" ", pad))
		col += pad
	}
	return out.String()
}

// sanitizeInspectLine makes one decoded line safe to write to a terminal.
// Raw mode needs no equivalent: docker's JSON escapes a control byte into a
// six-character backslash-u-0-0-1-b sequence, so it renders as text. The
// summary decodes them back into real bytes, and an ENV value or a probe is
// attacker-influenceable — a third-party image can carry an OSC 52 clipboard
// write or a report/paste sequence, and ansi.StringWidth counts an escape
// sequence as zero cells so both the wrap and the viewport pass it straight
// through. ansi.Strip removes the escape sequences; the rune filter removes
// what it leaves behind (BEL, CR, DEL, and the 8-bit C1 controls a terminal
// reads as escape introducers). Tabs and newlines are dropped too: kv and
// block split on the newline and expandTabs has already run, so a survivor
// here is by definition not something to write out.
func sanitizeInspectLine(line string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, ansi.Strip(line))
}

// wrapCells breaks one line into chunks of at most width display CELLS.
// softWrapLine chunks by RUNE, which overruns the pane by up to 2x for a wide
// grapheme (CJK, emoji) — and a probe output, an env value or a mount path can
// carry either, so the never-exceed-width invariant this screen promises has to
// be measured the same way it is rendered. Nothing is dropped: the chunks
// concatenate back to the input.
//
// A single wide grapheme still occupies 2 cells, so a width of 1 is the one
// case that cannot hold.
func wrapCells(line string, width int) []string {
	// Before the measurement, never after it: a tab expanded post-wrap would
	// widen a chunk the wrap already declared to fit.
	line = expandTabs(line)
	if width <= 0 || ansi.StringWidth(line) <= width {
		return []string{line}
	}
	return strings.Split(ansi.Hardwrap(line, width, true), "\n")
}

// inspectBuilder accumulates the summary line by line. Every push goes through
// a soft wrap at the target width, so the caller never has to think about the
// never-exceed-width invariant.
type inspectBuilder struct {
	width int
	lines []string
}

// push appends one already-fitted line, dropping trailing padding. Every byte
// docker decoded lands here, so this is the one chokepoint where a line is made
// safe to write to a terminal — see sanitizeInspectLine.
func (b *inspectBuilder) push(line string) {
	b.lines = append(b.lines, strings.TrimRight(sanitizeInspectLine(line), " "))
}

// section starts a named block, separated from the previous one by a blank line.
// The title line bypasses push deliberately: it carries our own lipgloss style,
// which IS an escape sequence, and push exists to strip those out.
func (b *inspectBuilder) section(title string) {
	if len(b.lines) > 0 {
		b.push("")
	}
	b.lines = append(b.lines, clampToWidth("  "+helpGroupTitleStyle.Render(title), b.width))
}

// kv writes a "label   value" row. The value is soft-wrapped, with continuation
// lines aligned under the value column; it is never truncated.
func (b *inspectBuilder) kv(label, value string) {
	head := "  " + label
	if n := ansi.StringWidth(head); n < inspectValueCol {
		head += strings.Repeat(" ", inspectValueCol-n)
	} else {
		head += " "
	}

	indent := ansi.StringWidth(head)
	if b.width-indent < inspectMinValueWidth {
		for _, chunk := range wrapCells(strings.TrimRight(head, " "), b.width) {
			b.push(chunk)
		}
		b.block(inspectBlockIndent, value)
		return
	}

	pad := strings.Repeat(" ", indent)
	first := true
	for _, para := range strings.Split(value, "\n") {
		for _, chunk := range wrapCells(para, b.width-indent) {
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
	// The guard above leaves indent < b.width, so avail is at least 1.
	avail := b.width - indent
	pad := strings.Repeat(" ", indent)
	for _, para := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for _, chunk := range wrapCells(strings.TrimRight(para, " \t"), avail) {
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
	inspectImageSection(b, doc)
	inspectMountsSection(b, doc)
	inspectEnvSection(b, doc)
	return b.String()
}

func inspectStateSection(b *inspectBuilder, doc compose.InspectDoc) {
	b.section("STATE")

	// The breadcrumb names the SERVICE; on a scaled service this row is the
	// only place the summary says which replica was picked.
	if doc.Name != "" {
		b.kv("container", doc.Name)
	}

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
	// docker's own reason the container failed — on a container that never
	// started this is the whole answer, and the exit code alone is not.
	if errText := strings.TrimSpace(doc.State.Error); errText != "" {
		b.kv("error", errText)
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

	// The rows are built into a scratch builder first: hasHealthcheck answers
	// "is there a healthcheck", which a zero-valued &InspectHealthcheck{} with
	// no runtime state satisfies while having nothing at all to say. A section
	// with nothing to say is omitted, so the header is only written once at
	// least one row exists.
	rows := &inspectBuilder{width: b.width}
	if state != nil {
		status := state.Status
		if status == "" {
			status = "unknown"
		}
		rows.kv("status", status)
		rows.kv("failing streak", strconv.Itoa(state.FailingStreak))
	}
	if hc != nil {
		if test := strings.Join(hc.Test, " "); test != "" {
			rows.kv("test", test)
		}
		if hc.Interval > 0 {
			rows.kv("interval", hc.Interval.String())
		}
		if hc.Timeout > 0 {
			rows.kv("timeout", hc.Timeout.String())
		}
		if hc.StartPeriod > 0 {
			rows.kv("start period", hc.StartPeriod.String())
		}
		if hc.Retries > 0 {
			rows.kv("retries", strconv.Itoa(hc.Retries))
		}
	}
	if probe, ok := lastHealthProbe(state); ok {
		rows.kv("last probe", formatProbeHeader(probe))
		if out := strings.TrimRight(probe.Output, "\n"); out != "" {
			rows.block(inspectBlockIndent, out)
		}
	}

	if len(rows.lines) == 0 {
		return
	}
	b.section("HEALTH")
	b.lines = append(b.lines, rows.lines...)
}

// inspectImageSection renders what the container actually runs: the ref the
// compose file asked for, the digest docker resolved it to, and the command
// pair. The digest is the row that answers "did my deploy take?" — a stale
// container keeps the old digest under an unchanged tag.
func inspectImageSection(b *inspectBuilder, doc compose.InspectDoc) {
	cmd := strings.Join(doc.Config.Cmd, " ")
	entrypoint := strings.Join(doc.Config.Entrypoint, " ")
	if doc.Config.Image == "" && doc.Image == "" && cmd == "" && entrypoint == "" {
		return
	}
	b.section("IMAGE")

	if doc.Config.Image != "" {
		b.kv("image", doc.Config.Image)
	}
	if doc.Image != "" {
		b.kv("digest", doc.Image)
	}
	if cmd != "" {
		b.kv("command", cmd)
	}
	if entrypoint != "" {
		b.kv("entrypoint", entrypoint)
	}
}

func inspectMountsSection(b *inspectBuilder, doc compose.InspectDoc) {
	if len(doc.Mounts) == 0 {
		return
	}
	b.section("MOUNTS")

	for _, m := range doc.Mounts {
		label := m.Type
		if label == "" {
			label = "mount"
		}
		b.kv(label, formatInspectMount(m))
	}
}

// formatInspectMount renders one mount as "source → destination  rw". The arrow
// is U+2192, the same one FormatPort uses, so the two columns read alike.
func formatInspectMount(m compose.InspectMount) string {
	source := m.Source
	if source == "" {
		// An anonymous volume has no bind source; its generated name is all
		// there is to identify it by.
		source = m.Name
	}
	if source == "" {
		source = "(unnamed)"
	}
	access := "ro"
	if m.RW {
		access = "rw"
	}
	return source + " → " + m.Destination + "  " + access
}

// inspectEnvSection renders the container's environment verbatim, secrets
// included — see the no-masking decision in the README. These are the values the
// RUNNING container holds, which is the whole point: a compose file edited after
// an `r` restart still shows the new value while the container holds the old one.
func inspectEnvSection(b *inspectBuilder, doc compose.InspectDoc) {
	if len(doc.Config.Env) == 0 {
		return
	}
	b.section("ENV")

	for _, e := range doc.Config.Env {
		if strings.TrimSpace(e) == "" {
			continue
		}
		b.block(inspectListIndent, e)
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
