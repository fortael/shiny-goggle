package profiler

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	gutter   = "  "
	minWidth = 52

	defaultSlowRows     = 10
	minSlowRows         = 5
	maxSlowRows         = 10
	defaultRecentRows   = 5
	maxRecentRows       = 10
	defaultBlockingRows = 5
	maxBlockingRows     = 10

	fullRunRows      = 6
	fullTimelineRows = 6
	simpleRunRows    = 3
	maxDiagRows      = 3
)

var (
	spinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}
	sparkRunes    = []rune("▁▂▃▄▅▆▇█")
)

type styles struct {
	title   lipgloss.Style
	dim     lipgloss.Style
	faint   lipgloss.Style
	ok      lipgloss.Style
	warn    lipgloss.Style
	bad     lipgloss.Style
	accent  lipgloss.Style
	section lipgloss.Style

	// Timeline slices are drawn as filled rectangles, like a trace viewer.
	sliceCool lipgloss.Style
	sliceMid  lipgloss.Style
	sliceWarm lipgloss.Style
	sliceHot  lipgloss.Style
	sliceLive lipgloss.Style
}

func newStyles() *styles {
	r := lipgloss.NewRenderer(os.Stderr)

	return &styles{
		title:   r.NewStyle().Bold(true),
		dim:     r.NewStyle().Foreground(lipgloss.Color("245")),
		faint:   r.NewStyle().Foreground(lipgloss.Color("240")),
		ok:      r.NewStyle().Foreground(lipgloss.Color("42")),
		warn:    r.NewStyle().Foreground(lipgloss.Color("214")),
		bad:     r.NewStyle().Foreground(lipgloss.Color("203")),
		accent:  r.NewStyle().Foreground(lipgloss.Color("39")),
		section: r.NewStyle().Foreground(lipgloss.Color("246")).Bold(true),

		sliceCool: r.NewStyle().Background(lipgloss.Color("24")).Foreground(lipgloss.Color("253")),
		sliceMid:  r.NewStyle().Background(lipgloss.Color("30")).Foreground(lipgloss.Color("231")),
		sliceWarm: r.NewStyle().Background(lipgloss.Color("136")).Foreground(lipgloss.Color("232")),
		sliceHot:  r.NewStyle().Background(lipgloss.Color("124")).Foreground(lipgloss.Color("231")),
		sliceLive: r.NewStyle().Background(lipgloss.Color("97")).Foreground(lipgloss.Color("231")),
	}
}

// durStyle colours a duration by how much it should worry you.
func (s *styles) durStyle(d time.Duration) lipgloss.Style {
	switch {
	case d >= 3*time.Second:
		return s.bad
	case d >= time.Second:
		return s.warn
	default:
		return s.ok
	}
}

func (s *styles) sliceStyle(sg timeSlice) lipgloss.Style {
	if sg.live {
		return s.sliceLive
	}

	switch {
	case sg.act.dur >= 3*time.Second:
		return s.sliceHot
	case sg.act.dur >= time.Second:
		return s.sliceWarm
	case sg.act.dur >= 300*time.Millisecond:
		return s.sliceMid
	default:
		return s.sliceCool
	}
}

// layout is the vertical budget of the verbose screen. It depends only on the
// terminal size and the options — never on the data — so panels keep their
// height while the build fills them in and nothing jumps around.
type layout struct {
	run      int
	blocking int
	slow     int
	timeline int
	recent   int
}

func newLayout(height, top, recent, blocking int) layout {
	l := layout{
		run:      fullRunRows,
		blocking: blocking,
		slow:     top,
		timeline: fullTimelineRows,
		recent:   recent,
	}

	// header + blank + keys, then two border lines per panel in use.
	cost := func() int {
		n := 4
		for _, rows := range []int{l.run, l.blocking, l.slow, l.timeline, l.recent} {
			if rows > 0 {
				n += rows + 2
			}
		}

		return n
	}

	avail := max(height-1, 8)

	// Give up the least useful space first: the "done" list is also visible in
	// the timeline, the timeline degrades gracefully, and the two leaderboards
	// — what costs the most and what nobody could compile around — are the
	// whole point of the screen.
	for _, step := range []struct {
		field *int
		floor int
	}{
		{&l.recent, 0},
		{&l.timeline, 3},
		{&l.run, 3},
		{&l.blocking, 3},
		{&l.slow, minSlowRows},
		{&l.timeline, 0},
		{&l.blocking, 0},
		{&l.slow, 3},
		{&l.run, 1},
		{&l.slow, 0},
	} {
		for cost() > avail && *step.field > step.floor {
			*step.field--
		}

		if cost() <= avail {
			break
		}
	}

	return l
}

// renderLive draws the verbose screen: what is running now (with the duration
// each package usually takes), the slowest packages so far and a trace-viewer
// style timeline of everything that has been built.
func (m *model) renderLive() string {
	s, sn := m.styles, m.sn
	width := max(m.width, minWidth)
	inner := width - len(gutter)
	l := newLayout(m.height, m.opts.top, m.opts.recent, m.opts.blocking)

	out := make([]string, 0, m.height)
	out = append(out, m.headerLines(inner)...)
	out = append(out, "")

	if l.run > 0 {
		rows := make([]string, 0, l.run)
		for _, a := range sn.activeRows(l.run) {
			rows = append(rows, m.runningRow(a, inner-4))
		}
		if len(rows) == 0 && !sn.done {
			rows = append(rows, s.faint.Render("nothing in flight — the go command is resolving dependencies"))
		}

		spark := s.faint.Render("parallelism ") + s.accent.Render(sparkline(sn.samples, min(24, inner/4)))
		out = append(out, s.panel(m.runningTitle(), spark, rows, inner, l.run)...)
	}

	if l.blocking > 0 {
		out = append(out, s.panel(
			s.section.Render("BLOCKING")+s.faint.Render(" · alone in the build, everything else waited"),
			s.faint.Render(serialNote(sn)),
			blockingRows(s, sn.visibleBlocking(l.blocking), inner-4),
			inner, l.blocking,
		)...)
	}

	if l.slow > 0 {
		out = append(out, s.panel(
			s.section.Render("SLOWEST"),
			s.faint.Render(concentrationNote(sn, m.opts.top)),
			chartRows(s, sn.visibleSlow(m.showStd, l.slow), inner-4),
			inner, l.slow,
		)...)
	}

	if l.timeline > 0 {
		out = append(out, s.panel(
			s.section.Render("TIMELINE")+s.faint.Render(" · every action, packed by lane"),
			s.faint.Render("0 → "+fmtDur(sn.elapsed)),
			timelineRows(s, sn, inner-4, l.timeline),
			inner, l.timeline,
		)...)
	}

	if l.recent > 0 {
		// While `go test` runs, results are far more interesting than the tail
		// of the compile log — packages finish testing long after they compiled.
		if m.opts.testing {
			out = append(out, s.panel(
				s.section.Render("TESTS"), s.faint.Render(diagNote(sn)),
				testRows(s, sn, l.recent, inner-4), inner, l.recent,
			)...)
		} else {
			rows := make([]string, 0, l.recent)
			for _, a := range sn.visibleRecent(m.showStd, l.recent) {
				rows = append(rows, doneRow(s, a, inner-4))
			}
			out = append(out, s.panel(
				s.section.Render("DONE")+s.faint.Render(fmt.Sprintf(" %d", sn.totalPackages())),
				s.faint.Render(diagNote(sn)),
				rows, inner, l.recent,
			)...)
		}
	}

	out = append(out, gutter+m.keyHints())

	return strings.Join(out, "\n")
}

// renderSimple is the quiet screen: a spinner, what is compiling right now and
// how long the build has been running. Nothing else is collected or shown.
func (m *model) renderSimple() string {
	s, sn := m.styles, m.sn
	width := max(m.width, minWidth)
	inner := width - len(gutter)

	out := []string{
		gutter + twoCol(
			m.statusMark()+" "+s.title.Render(truncateRight(m.title, inner-16)),
			s.title.Render(fmtDur(sn.elapsed)), inner),
		gutter + "  " + m.progressBar(clamp(inner/3, 12, 30)) + "  " + s.dim.Render(m.packageCount()),
	}

	// A fixed number of rows: the block must not jump while packages come and go.
	rows := sn.activeRows(simpleRunRows)
	for i := range simpleRunRows {
		if i >= len(rows) {
			out = append(out, "")

			continue
		}

		d := rows[i].running(sn.now)
		out = append(out, gutter+"   "+twoCol(
			s.dim.Render(truncate(rows[i].pkg, inner-16)),
			s.durStyle(d).Render(fmtDur(d)),
			inner-3,
		))
	}

	return strings.Join(out, "\n")
}

// statusMark is the spinner while the build runs and its verdict once it stops.
func (m *model) statusMark() string {
	s, sn := m.styles, m.sn

	switch {
	case m.stopping && !sn.done:
		return s.bad.Render("■")
	case sn.done && (sn.failed > 0 || sn.failedGo):
		return s.bad.Render("✗")
	case sn.done:
		return s.ok.Render("✓")
	default:
		return s.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])
	}
}

func (m *model) headerLines(inner int) []string {
	s, sn := m.styles, m.sn

	elapsed := s.title.Render(fmtDur(sn.elapsed))
	if m.eta > 0 && !sn.done {
		elapsed += s.faint.Render(" / ~" + fmtDur(m.eta))
	}

	line1 := gutter + twoCol(
		m.statusMark()+" "+s.title.Render(truncateRight(m.title, inner-24)), elapsed, inner)

	return []string{line1, gutter + twoCol(m.progressBar(clamp(inner/3, 12, 34)), m.counters(), inner)}
}

// progressBar prefers the honest denominator — the packages the go command said
// it would compile — and only falls back to the time estimate, then to a pulse.
func (m *model) progressBar(width int) string {
	s, sn := m.styles, m.sn

	if sn.done {
		return s.ok.Render(strings.Repeat("█", width))
	}

	frac, known := sn.progress()
	if !known {
		if m.eta <= 0 {
			return pulseBar(s, width, m.frame)
		}
		frac = sn.elapsed.Seconds() / m.eta.Seconds()
	}

	frac = clamp01(frac)
	filled := int(frac * float64(width))

	return s.accent.Render(strings.Repeat("█", filled)) +
		s.faint.Render(strings.Repeat("░", width-filled)) +
		s.dim.Render(fmt.Sprintf(" %3.0f%%", frac*100))
}

// packageCount reads "142/258 pkgs" once the build plan is known.
func (m *model) packageCount() string {
	if m.sn.planned > 0 {
		return fmt.Sprintf("%d/%d pkgs", m.sn.totalPackages(), m.sn.planned)
	}

	return fmt.Sprintf("%d pkgs", m.sn.totalPackages())
}

func (m *model) counters() string {
	s, sn := m.styles, m.sn

	parts := []string{
		s.title.Render(m.packageCount()),
		s.title.Render(fmt.Sprint(len(sn.active))) + s.dim.Render(" running"),
	}
	if p := sn.parallelism(); p > 0 {
		parts = append(parts, s.dim.Render(fmt.Sprintf("%.1f× parallel", p)))
	}
	if sn.failed > 0 {
		parts = append(parts, s.bad.Render(fmt.Sprintf("%d failed", sn.failed)))
	}

	return strings.Join(parts, s.faint.Render(" · "))
}

func (m *model) runningTitle() string {
	s := m.styles
	if m.sn.done {
		return s.section.Render("RUNNING") + s.faint.Render(" · finished")
	}

	return s.section.Render("RUNNING") + s.faint.Render(fmt.Sprintf(" %d", len(m.sn.active)))
}

func (m *model) keyHints() string {
	s := m.styles

	switch {
	case m.sn.done:
		return s.faint.Render("screen kept — the report follows below")
	case m.stopping:
		return s.bad.Render("stopping…") + s.faint.Render("  press q again to detach")
	default:
		stdlib := " hide stdlib"
		if !m.showStd {
			stdlib = " show stdlib"
		}

		return s.faint.Render("q") + s.dim.Render(" stop") +
			s.faint.Render("  ·  v") + s.dim.Render(stdlib)
	}
}

func (m *model) runningRow(a action, width int) string {
	s := m.styles
	d := a.running(m.sn.now)

	dur := s.durStyle(d).Render(padLeft(fmtDur(d), 7))

	hint := ""
	if a.expect > 0 {
		hint = s.faint.Render("~" + fmtDur(a.expect))
	}

	// Nothing else is compiling: this one package is the build right now.
	marker := s.accent.Render("▸")
	if a.blocking() {
		marker = s.bad.Render("⊘")
		hint = s.bad.Render("blocking "+fmtDur(a.solo)) + " " + hint
	}

	name := a.pkg
	if name == "" {
		name = "(no package)"
	}

	prefix := ""
	if a.tool != "compile" {
		prefix = s.warn.Render(a.tool) + " "
	}

	room := width - 12 - lipgloss.Width(prefix) - lipgloss.Width(hint)
	name = truncate(name, room)
	if a.std {
		name = s.faint.Render(name) // stdlib is noise on a cold build
	}

	return twoCol(marker+" "+dur+"  "+prefix+name, hint, width)
}

// testRows lists the most recent package verdicts, failures kept in view.
func testRows(s *styles, sn snap, n, width int) []string {
	if len(sn.tests) == 0 {
		return []string{s.faint.Render("compiling — no package has finished testing yet")}
	}

	rows := make([]string, 0, n)

	// Failures first, then the newest results: a red line must not scroll away.
	for _, r := range sn.tests {
		if r.status == testFailed && len(rows) < n {
			rows = append(rows, testRow(s, r, width))
		}
	}

	for i := len(sn.tests) - 1; i >= 0 && len(rows) < n; i-- {
		if sn.tests[i].status != testFailed {
			rows = append(rows, testRow(s, sn.tests[i], width))
		}
	}

	return rows
}

func testRow(s *styles, r testResult, width int) string {
	icon, note := s.ok.Render("✓"), s.durStyle(r.dur).Render(padLeft(fmtDur(r.dur), 7))

	switch r.status {
	case testFailed:
		icon = s.bad.Render("✗")
	case testCached:
		icon, note = s.ok.Render("✓"), s.faint.Render(padLeft("cached", 7))
	case testNoTests:
		icon, note = s.faint.Render("·"), s.faint.Render(padLeft("no tests", 8))
	case testPassed:
	}

	return icon + " " + note + "  " + s.dim.Render(truncate(r.pkg, width-14))
}

func doneRow(s *styles, a *action, width int) string {
	icon := s.ok.Render("✓")
	if a.failed {
		icon = s.bad.Render("✗")
	}

	name := a.pkg
	if a.tool != "compile" {
		name = s.warn.Render(a.tool) + " " + name
	}

	return icon + " " + s.durStyle(a.dur).Render(padLeft(fmtDur(a.dur), 7)) +
		"  " + s.dim.Render(truncate(name, width-12))
}

// blockingRows lists the packages the build had to wait for on its own. The bar
// is the solo time, the number on the right is how much of it that was.
func blockingRows(s *styles, top []*action, width int) []string {
	if len(top) == 0 {
		return []string{s.faint.Render("nothing has held the build on its own yet")}
	}

	maxSolo := top[0].solo
	if maxSolo <= 0 {
		maxSolo = time.Millisecond
	}

	barW := clamp(width/4, 8, 22)
	nameW := width - barW - 26

	rows := make([]string, 0, len(top))
	for _, a := range top {
		filled := clamp(int(float64(a.solo)/float64(maxSolo)*float64(barW)), 1, barW)

		label := ""
		if a.tool != "compile" {
			label = s.warn.Render(a.tool) + " "
		}

		// The total tells you whether the package is slow or merely lonely.
		total := ""
		if a.dur > 0 {
			total = s.faint.Render("of " + fmtDur(a.dur))
		}

		left := s.bad.Render(padLeft(fmtDur(a.solo), 7)) + " " +
			s.bad.Render(strings.Repeat("█", filled)) + s.faint.Render(strings.Repeat("·", barW-filled)) +
			"  " + label + s.dim.Render(truncate(a.pkg, nameW-lipgloss.Width(label)))

		rows = append(rows, twoCol(left, total, width))
	}

	return rows
}

// chartRows renders the horizontal top-N bar chart.
func chartRows(s *styles, top []*action, width int) []string {
	if len(top) == 0 {
		return nil
	}

	maxDur := top[0].dur
	if maxDur <= 0 {
		maxDur = time.Millisecond
	}

	barW := clamp(width/3, 10, 30)
	nameW := width - barW - 12

	rows := make([]string, 0, len(top))
	for _, a := range top {
		filled := clamp(int(float64(a.dur)/float64(maxDur)*float64(barW)), 1, barW)

		bar := s.durStyle(a.dur).Render(strings.Repeat("█", filled)) +
			s.faint.Render(strings.Repeat("·", barW-filled))

		// A package can appear twice (compile and link); say which one it is.
		label := ""
		if a.tool != "compile" {
			label = s.warn.Render(a.tool) + " "
		}

		rows = append(rows, s.durStyle(a.dur).Render(padLeft(fmtDur(a.dur), 7))+
			" "+bar+"  "+label+s.dim.Render(truncate(a.pkg, nameW-lipgloss.Width(label))))
	}

	return rows
}

// panel draws a titled box of exactly height+2 lines and `width` columns, so
// its size is decided by the layout and not by how much data there is yet.
func (s *styles) panel(title, right string, rows []string, width, height int) []string {
	inner := width - 4

	head := " " + title + " "

	// Styled but empty strings still carry escape codes, so measure the width.
	tail := ""
	if lipgloss.Width(right) > 0 {
		tail = " " + ansi.Truncate(right, max(inner-lipgloss.Width(head), 0), "") + " "
	}

	fill := max(width-2-lipgloss.Width(head)-lipgloss.Width(tail), 0)

	out := make([]string, 0, height+2)
	out = append(out, gutter+s.faint.Render("╭")+head+s.faint.Render(strings.Repeat("─", fill))+tail+s.faint.Render("╮"))

	for i := range height {
		row := ""
		if i < len(rows) {
			row = rows[i]
		}
		out = append(out, gutter+s.faint.Render("│")+" "+fit(row, inner)+" "+s.faint.Render("│"))
	}

	return append(out, gutter+s.faint.Render("╰"+strings.Repeat("─", width-2)+"╯"))
}

// renderSummary is the report printed under the screen once the build is over.
func renderSummary(sn snap, g *buildGraph, opts *options, width int) string {
	s := newStyles()
	width = max(width, minWidth)

	// -clear asked for the screen to disappear; following it with a full report
	// would defeat the point, so it gets the one line version too.
	if !opts.verbose() || opts.clear {
		return simpleSummary(s, sn)
	}

	return verboseSummary(s, sn, g, opts, width-len(gutter))
}

// criticalPathSection is the one answer watching tools cannot give: the chain of
// packages that had to happen one after another, and therefore set the clock.
func criticalPathSection(s *styles, g *buildGraph, inner int) []string {
	if g == nil || len(g.path) == 0 {
		return nil
	}

	note := fmt.Sprintf("%s of %s wall · %d links", fmtDur(g.critical), fmtDur(g.wall), len(g.path))

	out := []string{
		"",
		gutter + twoCol(
			s.section.Render("CRITICAL PATH")+s.faint.Render(" · nothing can overlap this chain"),
			s.faint.Render(note), inner),
	}

	// Sorting by duration rather than by order in the chain: the question is
	// what to attack, not what ran when.
	path := append([]link(nil), g.path...)
	sort.SliceStable(path, func(i, j int) bool { return path[i].dur > path[j].dur })

	maxDur := path[0].dur
	if maxDur <= 0 {
		maxDur = time.Millisecond
	}

	barW := clamp(inner/4, 8, 22)

	for i, l := range path {
		if i == 6 {
			out = append(out, gutter+s.faint.Render(fmt.Sprintf("  … and %d more links", len(path)-i)))

			break
		}

		filled := clamp(int(float64(l.dur)/float64(maxDur)*float64(barW)), 1, barW)
		label := ""
		if l.mode != "build" {
			label = s.warn.Render(l.mode) + " "
		}

		out = append(out, gutter+s.durStyle(l.dur).Render(padLeft(fmtDur(l.dur), 7))+" "+
			s.accent.Render(strings.Repeat("█", filled))+s.faint.Render(strings.Repeat("·", barW-filled))+
			"  "+label+s.dim.Render(truncate(l.pkg, inner-barW-14)))
	}

	verdict := fmt.Sprintf("%.0f%% of the build is this chain — more cores would save at most %s",
		g.criticalShare()*100, fmtDur(g.slack()))
	if g.criticalShare() < 0.5 {
		verdict = fmt.Sprintf("%.0f%% of the build is this chain — the rest is parallel work, %s waited for a free worker",
			g.criticalShare()*100, fmtDur(g.queueWait))
	}

	return append(out, gutter+s.faint.Render("→ "+verdict))
}

// rebuildSection explains why this build had anything to do at all.
func rebuildSection(s *styles, g *buildGraph, inner int) []string {
	if g == nil || g.actions == 0 {
		return nil
	}

	note := fmt.Sprintf("%d of %d packages rebuilt · %d served from the cache", g.built, g.built+g.cached, g.cached)
	out := []string{
		"",
		gutter + twoCol(s.section.Render("REBUILT"), s.faint.Render(note), inner),
	}

	if len(g.roots) == 0 {
		return append(out, gutter+s.faint.Render("  nothing was cached — this was a cold build"))
	}

	for _, r := range g.roots {
		downstream := s.faint.Render("only itself")
		if r.downstream > 0 {
			downstream = s.warn.Render(fmt.Sprintf("%d packages downstream", r.downstream))
		}

		out = append(out, gutter+"  "+s.bad.Render("changed")+" "+
			s.dim.Render(truncate(r.pkg, inner-34))+"  →  "+downstream)
	}

	return out
}

// toolSection breaks the build down by tool: linking and cgo are the costs
// nobody expects, and they are invisible in a per-package chart.
func toolSection(s *styles, sn snap, inner int) []string {
	if len(sn.toolTime) == 0 {
		return nil
	}

	type entry struct {
		tool string
		dur  time.Duration
		n    int
	}

	all := make([]entry, 0, len(sn.toolTime))
	for tool, d := range sn.toolTime {
		all = append(all, entry{tool, d, sn.tools[tool]})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].dur > all[j].dur })

	parts := make([]string, 0, len(all))
	for _, e := range all {
		parts = append(parts, s.title.Render(e.tool)+s.dim.Render(fmt.Sprintf(" %s", fmtDur(e.dur)))+
			s.faint.Render(fmt.Sprintf(" ×%d", e.n)))
	}

	return []string{"", gutter + twoCol(s.section.Render("BY TOOL"),
		s.faint.Render("cpu time, not wall clock"), inner),
		gutter + "  " + strings.Join(parts, s.faint.Render("  ·  "))}
}

// compilerSection opens up the compile step itself, from `compile -bench`:
// how the time split between type checking and code generation, and how much
// code the compiler actually generated. A package well above the build's own
// functions-per-line average is generating far more code than it contains —
// repeated generic instantiation is the usual reason.
func compilerSection(s *styles, sn snap, inner int) []string {
	if len(sn.heavy) == 0 {
		return nil
	}

	baseline := sn.density()

	out := []string{
		"",
		gutter + twoCol(
			s.section.Render("INSIDE THE COMPILER")+s.faint.Render(" · where the time went within a package"),
			s.faint.Render(fmt.Sprintf("build average %.2f funcs/line", baseline)), inner),
	}

	for i, a := range sn.heavy {
		if i == 6 {
			break
		}

		total := a.frontend + a.backend
		if total <= 0 {
			continue
		}

		frontShare := float64(a.frontend) / float64(total)
		split := s.accent.Render(fmt.Sprintf("%3.0f%% types", frontShare*100)) +
			s.faint.Render(" / ") + s.warn.Render(fmt.Sprintf("%3.0f%% codegen", (1-frontShare)*100))

		note := s.faint.Render(fmt.Sprintf("%d funcs / %d lines", a.funcs, a.lines))
		// Only call it out when it stands out against this very build.
		if baseline > 0 && a.density() > 2*baseline && a.funcs >= 100 {
			note = s.bad.Render(fmt.Sprintf("%d funcs / %d lines  ×%.1f typical",
				a.funcs, a.lines, a.density()/baseline))
		}

		out = append(out, gutter+s.durStyle(total).Render(padLeft(fmtDur(total), 7))+"  "+split+"  "+
			s.dim.Render(truncate(a.pkg, inner-52))+"  "+note)
	}

	return out
}

// testSection reports the run itself, which for `go test` dwarfs compiling.
func testSection(s *styles, sn snap, inner int) []string {
	if len(sn.tests) == 0 {
		return nil
	}

	passed, failed, skipped := testTally(sn.tests)

	tally := s.ok.Render(fmt.Sprintf("%d ok", passed))
	if failed > 0 {
		tally += s.faint.Render(" · ") + s.bad.Render(fmt.Sprintf("%d failed", failed))
	}
	if skipped > 0 {
		tally += s.faint.Render(fmt.Sprintf(" · %d without tests", skipped))
	}
	if sn.testFails > 0 {
		tally += s.faint.Render(" · ") + s.bad.Render(fmt.Sprintf("%d failing tests", sn.testFails))
	}

	out := []string{"", gutter + twoCol(s.section.Render("TESTS"), tally, inner)}

	slowest := append([]testResult(nil), sn.tests...)
	sort.SliceStable(slowest, func(i, j int) bool { return slowest[i].dur > slowest[j].dur })

	for i, r := range slowest {
		if i == 5 || r.dur <= 0 {
			break
		}
		out = append(out, gutter+s.durStyle(r.dur).Render(padLeft(fmtDur(r.dur), 7))+"  "+
			s.dim.Render(truncate(r.pkg, inner-14)))
	}

	for _, r := range sn.tests {
		if r.status == testFailed {
			out = append(out, gutter+s.bad.Render("✗ ")+s.dim.Render(truncate(r.pkg, inner-4)))
		}
	}

	return out
}

func simpleSummary(s *styles, sn snap) string {
	switch {
	case sn.failed > 0 || sn.failedGo:
		return gutter + s.bad.Render("✗ build failed") + s.dim.Render(" after "+fmtDur(sn.elapsed)) + "\n"
	case sn.totalPackages() == 0:
		return gutter + s.ok.Render("✓ nothing to compile") + s.dim.Render(" ("+fmtDur(sn.elapsed)+")") + "\n"
	default:
		return gutter + s.ok.Render("✓ built ") + s.title.Render(fmt.Sprint(sn.totalPackages())) +
			s.dim.Render(" packages in ") + s.title.Render(fmtDur(sn.elapsed)) + "\n"
	}
}

//nolint:funlen,gocyclo // a report with several optional blocks
func verboseSummary(s *styles, sn snap, g *buildGraph, opts *options, inner int) string {
	var out []string

	add := func(l string) { out = append(out, l) }

	add("")

	switch {
	case sn.failed > 0 || sn.failedGo:
		add(gutter + s.bad.Render("✗ build failed") + s.dim.Render(" after "+fmtDur(sn.elapsed)))
	case sn.totalPackages() == 0:
		add(gutter + s.ok.Render("✓ nothing to compile") +
			s.dim.Render(" — the build cache covered everything in "+fmtDur(sn.elapsed)))
	default:
		add(gutter + s.ok.Render("✓ build finished") + s.dim.Render(" in ") + s.title.Render(fmtDur(sn.elapsed)))
	}

	if sn.totalPackages() > 0 {
		facts := []string{s.title.Render(fmt.Sprint(sn.compiled)) + s.dim.Render(" packages")}
		if sn.std > 0 {
			facts = append(facts, s.dim.Render(fmt.Sprintf("%d stdlib", sn.std)))
		}
		facts = append(facts,
			s.dim.Render(fmtDur(sn.cpu)+" cpu"),
			s.dim.Render(fmt.Sprintf("%.1f× parallel", sn.parallelism())),
			s.dim.Render(fmt.Sprintf("peak %d", sn.peakPar)),
		)
		// Wall clock the go command spent loading packages and bookkeeping,
		// with no tool running at all: it explains the rest of the gap.
		if sn.idle > 200*time.Millisecond {
			facts = append(facts, s.dim.Render(fmtDur(sn.idle)+" idle"))
		}
		add(gutter + "  " + strings.Join(facts, s.faint.Render(" · ")))
	}

	out = append(out, testSection(s, sn, inner)...)
	out = append(out, criticalPathSection(s, g, inner)...)
	out = append(out, rebuildSection(s, g, inner)...)

	if blocked := sn.visibleBlocking(opts.blocking); len(blocked) > 0 {
		add("")
		add(gutter + twoCol(
			s.section.Render("BLOCKING")+s.faint.Render(" · nothing else could compile meanwhile"),
			s.faint.Render(serialNote(sn)), inner))

		for _, row := range blockingRows(s, blocked, inner) {
			add(gutter + row)
		}
	}

	if top := sn.visibleSlow(!opts.hideStd, opts.top); len(top) > 0 && sn.totalPackages() >= 3 {
		add("")
		add(gutter + twoCol(s.section.Render("SLOWEST PACKAGES"),
			s.faint.Render(concentrationNote(sn, opts.top)), inner))

		for _, row := range chartRows(s, top, inner) {
			add(gutter + row)
		}
	}

	out = append(out, compilerSection(s, sn, inner)...)
	out = append(out, toolSection(s, sn, inner)...)

	for i, a := range sn.slower {
		if i == 0 {
			add("")
		}
		if i == 3 {
			break
		}

		add(gutter + s.warn.Render("⚠ slower than usual  ") + s.dim.Render(truncate(a.pkg, inner-40)) +
			s.warn.Render("  "+fmtDur(a.dur)) + s.faint.Render(" vs ~"+fmtDur(a.expect)))
	}

	if sn.aborted > 0 {
		add(gutter + s.faint.Render(fmt.Sprintf("· %d action(s) were interrupted", sn.aborted)))
	}

	if opts.trace != "" {
		add("")
		add(gutter + s.faint.Render("⋯ trace: ") + s.accent.Render(opts.trace) +
			s.faint.Render("  →  open at https://ui.perfetto.dev"))
	}
	if opts.goTrace != "" {
		add(gutter + s.faint.Render("⋯ go's own trace: ") + s.accent.Render(opts.goTrace))
	}
	if opts.actiongraph != "" {
		add(gutter + s.faint.Render("⋯ action graph: ") + s.accent.Render(opts.actiongraph))
	}

	// The trailing newlines are not decoration: bubbletea erases the last line
	// of the final frame on its way out, so the screen above needs a spare one,
	// and the other keeps whatever comes next (errors, program output) apart.
	return strings.Join(out, "\n") + "\n\n"
}

// serialNote states how much of the wall clock the build spent single file.
func serialNote(sn snap) string {
	share := sn.serialShare()
	if share <= 0 {
		return ""
	}

	return fmt.Sprintf("%.0f%% of the build (%s) ran single file", share*100, fmtDur(sn.serial))
}

func concentrationNote(sn snap, top int) string {
	c := sn.concentration(top)
	if c <= 0 {
		return ""
	}

	return fmt.Sprintf("%.0f%% of compile time in top %d", c*100, top)
}

// diagNote surfaces the last thing the go command said, if it said anything.
func diagNote(sn snap) string {
	for i := len(sn.diag) - 1; i >= 0 && i >= len(sn.diag)-maxDiagRows; i-- {
		if line := strings.TrimSpace(sn.diag[i]); line != "" {
			return truncateRight(line, 46)
		}
	}

	return ""
}

// sparkline compresses the whole parallelism history into width cells, so the
// shape of the build graph stays visible from the first package to the last.
func sparkline(samples []uint16, width int) string {
	if len(samples) == 0 || width <= 0 {
		return ""
	}

	buckets := make([]uint16, width)

	var peak uint16

	for i, v := range samples {
		b := i * width / len(samples)
		if v > buckets[b] {
			buckets[b] = v
		}
		if v > peak {
			peak = v
		}
	}

	if peak == 0 {
		peak = 1
	}

	var sb strings.Builder
	for _, v := range buckets {
		idx := int(v) * (len(sparkRunes) - 1) / int(peak)
		sb.WriteRune(sparkRunes[clamp(idx, 0, len(sparkRunes)-1)])
	}

	return sb.String()
}

// pulseBar is the "we have no idea how long this takes" indicator.
func pulseBar(s *styles, width, frame int) string {
	seg := max(width/4, 3)
	span := max(2*(width-seg), 1)

	pos := frame % span
	if pos >= width-seg {
		pos = span - pos
	}

	return s.faint.Render(strings.Repeat("░", pos)) +
		s.accent.Render(strings.Repeat("█", seg)) +
		s.faint.Render(strings.Repeat("░", max(width-seg-pos, 0)))
}

// twoCol lays out a left and a right chunk on one line of the given width.
func twoCol(left, right string, width int) string {
	if lipgloss.Width(right) == 0 {
		return left
	}

	pad := width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		return left + " " + right
	}

	return left + strings.Repeat(" ", pad) + right
}

// fit pads or truncates a rendered line to exactly width visible columns.
func fit(s string, width int) string {
	switch w := lipgloss.Width(s); {
	case w > width:
		return ansi.Truncate(s, width, "")
	case w < width:
		return s + strings.Repeat(" ", width-w)
	default:
		return s
	}
}

func truncate(s string, width int) string {
	if width <= 1 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}

	// Import paths read best from the right: keep the tail, elide the prefix.
	if parts := strings.Split(s, "/"); len(parts) > 1 {
		for i := 1; i < len(parts); i++ {
			candidate := "…/" + strings.Join(parts[i:], "/")
			if lipgloss.Width(candidate) <= width {
				return candidate
			}
		}
	}

	return truncateRight(s, width)
}

// truncateRight elides the tail; used for command lines and messages, where the
// beginning carries the meaning.
func truncateRight(s string, width int) string {
	if width <= 1 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= width {
		return s
	}

	return string(runes[:width-1]) + "…"
}

// floatSparkline is sparkline for measured values rather than counts; the scale
// always starts at zero so the shape is comparable between refreshes.
func floatSparkline(values []float64, width int) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}

	if len(values) > width {
		values = values[len(values)-width:]
	}

	var peak float64
	for _, v := range values {
		peak = max(peak, v)
	}

	if peak <= 0 {
		return strings.Repeat(string(sparkRunes[0]), len(values))
	}

	var sb strings.Builder
	for _, v := range values {
		idx := int(v / peak * float64(len(sparkRunes)-1))
		sb.WriteRune(sparkRunes[clamp(idx, 0, len(sparkRunes)-1)])
	}

	return sb.String()
}

func padRight(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}

	return s
}

func padLeft(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}

	return s
}

func fmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds()) // gc pauses live down here
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func clamp01(v float64) float64 {
	return min(max(v, 0), 0.99)
}
