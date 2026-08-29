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
)

// unsafeTerminalRune reports whether a rune must never reach the terminal: the
// C0 controls, DEL, and the 8-bit C1 block a terminal reads as escape
// introducers (U+009B is CSI, U+009D is OSC). Both inspect buffers filter
// through it, so the summary and raw mode cannot disagree about what is safe.
func unsafeTerminalRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// sanitizeInspectLine makes one decoded line safe to write to a terminal.
// docker's JSON escapes a control byte into a six-character
// backslash-u-0-0-1-b sequence, but ONLY the C0 block: Go's encoding/json
// emits DEL and the C1 code points as raw bytes, so raw mode needs its own
// pass too (see sanitizeInspectRaw). The summary decodes even the escaped C0
// back into real bytes, and an ENV value or a probe is attacker-influenceable
// — a third-party image can carry an OSC 52 clipboard write or a report/paste
// sequence, and ansi.StringWidth counts an escape sequence as zero cells so
// both the wrap and the viewport pass it straight through. ansi.Strip removes
// the escape sequences; the rune filter removes what it leaves behind (BEL,
// CR, DEL, and the C1 controls). Tabs and newlines are dropped too: kv and
// block split on the newline and expandTabs has already run, so a survivor
// here is by definition not something to write out.
func sanitizeInspectLine(line string) string {
	return strings.Map(func(r rune) rune {
		if unsafeTerminalRune(r) {
			return -1
		}
		return r
	}, ansi.Strip(line))
}

// sanitizeInspectRaw makes the raw docker inspect payload safe to write to a
// terminal. Go's encoding/json — which produces this payload on both the
// daemon and the CLI side — escapes only the C0 block, so an ENV value, a
// probe output or an image ref can still carry a raw DEL or a C1 escape
// introducer (U+009B, U+009D) straight through `i` then `r`. Newlines are the
// one control kept: they are the raw view's line structure.
//
// ansi.Strip is deliberately NOT run here. The ESC bytes arrive already
// escaped as the six printable characters backslash-u-0-0-1-b, so there is no
// escape sequence left to strip, and stripping would corrupt a payload that is
// safe as it stands — the raw view stays byte-identical to `docker inspect`
// for everything JSON already escapes.
func sanitizeInspectRaw(raw []byte) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unsafeTerminalRune(r) {
			return -1
		}
		return r
	}, string(raw))
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

// inspectUpdateInfo carries the update-detection half of the IMAGE section into
// the builder. The clock arrives as a field rather than a time.Now() call inside
// the renderer, so buildInspectSummary stays pure and the relative ages are
// testable against a fixed now.
//
// The ZERO VALUE draws no extra row: a caller with no cache entry in hand passes
// inspectUpdateInfo{} and gets exactly the summary this screen rendered before
// the update rows existed.
type inspectUpdateInfo struct {
	now       time.Time
	detail    compose.UpdateDetail
	verdict   *bool
	checkedAt time.Time
}

// buildInspectSummary renders the curated summary of one container's
// `docker inspect` output. Pure — no Model state, no TTY and no Docker — so it
// is golden-testable against the real fixtures in internal/compose/testdata.
//
// A section with nothing to say is omitted; STATE always renders.
func buildInspectSummary(doc compose.InspectDoc, width int, upd inspectUpdateInfo) string {
	if width <= 0 {
		width = inspectDefaultWidth
	}
	b := &inspectBuilder{width: width}
	inspectStateSection(b, doc)
	inspectHealthSection(b, doc)
	inspectImageSection(b, doc, upd)
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
// compose file asked for, the local image ID docker resolved it to, when that
// image was built, and the command pair. The image ID is the row that answers
// "did my deploy take?" — a stale container keeps the old image ID under an
// unchanged tag.
//
// `built` and the three update rows sit BETWEEN the image ID and the command
// pair, so the two ids and the two build dates read as one block: "image id"
// against "update id", "built" against "update built". Each row is omitted on
// its own, and the section's presence gate is unchanged — an update verdict
// describes the image, so a doc with no image at all still has nothing to say.
//
// `built` comes from the image probe alone, and a failed probe draws no row.
// upd.detail.LocalCreated is NOT a fallback for it: that one describes the tag
// the compose file declares, which a local pull can move off the image the
// container is still running.
func inspectImageSection(b *inspectBuilder, doc compose.InspectDoc, upd inspectUpdateInfo) {
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
		b.kv("image id", doc.Image)
	}
	if built := formatTimeWithAge(imageBuiltAt(doc), upd.now); built != "" {
		b.kv("built", built)
	}
	if upd.verdict != nil {
		b.kv("update", formatUpdateVerdict(*upd.verdict, upd.checkedAt, upd.now))
	}
	if upd.detail.NewID != "" {
		b.kv("update id", upd.detail.NewID)
	}
	if built := formatTimeWithAge(upd.detail.NewCreated, upd.now); built != "" {
		b.kv("update built", built)
	}
	if cmd != "" {
		b.kv("command", cmd)
	}
	if entrypoint != "" {
		b.kv("entrypoint", entrypoint)
	}
}

// imageBuiltAt is the source of the `built` row: the build date the inspect
// fetch probed for the container's RESOLVED image id. See inspectImageSection
// for why the update detail cannot stand in for it.
func imageBuiltAt(doc compose.InspectDoc) time.Time {
	return doc.ImageCreated
}

// formatUpdateVerdict renders the "update" row's value: the verdict, plus how
// old the check behind it is. The age suffix is dropped when the entry carries
// no fetch time, rather than reported as "moments ago" — a missing timestamp is
// not a fresh one.
func formatUpdateVerdict(available bool, checkedAt, now time.Time) string {
	value := "up to date"
	if available {
		value = "available"
	}
	if checkedAt.IsZero() {
		return value
	}
	return value + "  (checked " + humanizeAge(now.Sub(checkedAt)) + ")"
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
	// The blanks are dropped BEFORE the header is written, for the same reason
	// inspectHealthSection builds its rows into a scratch builder first: a
	// section with nothing to say is omitted, and an Env slice of nothing but
	// blank entries has nothing to say. A len(doc.Config.Env) != 0 gate reads
	// as if it answered that, and does not.
	entries := make([]string, 0, len(doc.Config.Env))
	for _, e := range doc.Config.Env {
		if strings.TrimSpace(e) == "" {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return
	}
	b.section("ENV")

	for _, e := range entries {
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
	return formatInspectTimeValue(t)
}

// formatInspectTimeValue renders an already-parsed timestamp in the same layout
// as formatInspectTime. One guard covers both the docker zero time (year 1, so
// far below the epoch) and 1970 itself: each is an absent value rather than
// data — the latter is what reproducible builders (distroless, ko, Bazel, nix)
// write into an image's Created field — so each yields "" and the caller omits
// the row.
func formatInspectTimeValue(t time.Time) string {
	if t.Unix() <= 0 {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

// formatTimeWithAge renders "2026-07-07 17:47:22  (47d ago)" relative to now.
// The clock is a parameter so the caller stays pure and testable.
func formatTimeWithAge(t, now time.Time) string {
	stamp := formatInspectTimeValue(t)
	if stamp == "" {
		return ""
	}
	return stamp + "  (" + humanizeAge(now.Sub(t)) + ")"
}
