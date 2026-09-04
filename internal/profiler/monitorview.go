package profiler

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

type monitorTab int

const (
	tabRuntime monitorTab = iota
	tabOutput
	tabProfile
)

type monitorTickMsg time.Time

// monitorModel is the screen you sit in front of while the program runs.
type monitorModel struct {
	st     *monitorState
	styles *styles
	stop   func()

	width, height int
	sized         bool
	frame         int
	tab           monitorTab
	scroll        int // lines from the bottom of the output
	follow        bool
	stopping      bool
	sn            monitorSnap
}

func newMonitorModel(st *monitorState, stop func()) *monitorModel {
	return &monitorModel{
		st:     st,
		styles: newStyles(),
		stop:   stop,
		width:  terminalWidth(),
		height: 30,
		follow: true,
		sn:     st.snapshot(),
	}
}

func (m *monitorModel) Init() tea.Cmd { return monitorTick2() }

func monitorTick2() tea.Cmd {
	return tea.Tick(monitorTick, func(t time.Time) tea.Msg { return monitorTickMsg(t) })
}

func (m *monitorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width, m.sized = msg.Width, true
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}

		return m, nil

	case monitorTickMsg:
		m.frame++
		m.sn = m.st.snapshot()

		if m.sn.exited {
			return m, tea.Quit
		}

		return m, monitorTick2()

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	return m, nil
}

func (m *monitorModel) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		if m.stopping {
			return m, tea.Quit // second press: stop waiting for a clean exit
		}
		m.stopping = true
		m.stop()

		return m, nil

	case "tab", "right":
		m.tab = (m.tab + 1) % 3
	case "shift+tab", "left":
		m.tab = (m.tab + 2) % 3
	case "1":
		m.tab = tabRuntime
	case "2":
		m.tab = tabOutput
	case "3":
		m.tab = tabProfile

	case "up", "k":
		m.scroll, m.follow = m.scroll+1, false
	case "down", "j":
		m.scroll = max(m.scroll-1, 0)
		m.follow = m.scroll == 0
	case "pgup":
		m.scroll, m.follow = m.scroll+m.height/2, false
	case "pgdown":
		m.scroll = max(m.scroll-m.height/2, 0)
		m.follow = m.scroll == 0
	case "g":
		m.scroll, m.follow = 0, true
	}

	return m, nil
}

func (m *monitorModel) View() string {
	width := max(m.width, minWidth)
	inner := width - len(gutter)

	out := append([]string{}, m.header(inner)...)

	switch m.tab {
	case tabRuntime:
		out = append(out, m.runtimeTab(inner)...)
	case tabOutput:
		out = append(out, m.outputTab(inner)...)
	case tabProfile:
		out = append(out, m.profileTab(inner)...)
	}

	out = append(out, gutter+m.footer())

	view := strings.Join(out, "\n")
	if m.sized {
		return view
	}

	lines := strings.Split(view, "\n")
	for i, l := range lines {
		if pad := m.width - lipgloss.Width(l); pad > 0 {
			lines[i] = l + strings.Repeat(" ", pad)
		}
	}

	return strings.Join(lines, "\n")
}

func (m *monitorModel) header(inner int) []string {
	s, sn := m.styles, m.sn

	mark := s.ok.Render("●")
	if m.stopping {
		mark = s.bad.Render("■")
	}

	right := s.dim.Render(fmt.Sprintf("pid %d", sn.pid)) +
		s.faint.Render(" · up ") + s.title.Render(fmtDur(sn.uptime))

	tabs := []string{"1 runtime", "2 output", "3 profile"}
	rendered := make([]string, 0, len(tabs))

	for i, t := range tabs {
		if monitorTab(i) == m.tab {
			rendered = append(rendered, s.accent.Render("▍"+t))

			continue
		}
		rendered = append(rendered, s.faint.Render(" "+t))
	}

	return []string{
		gutter + twoCol(mark+" "+s.title.Render(truncateRight(sn.cmdline, inner-24)), right, inner),
		gutter + strings.Join(rendered, s.faint.Render("  ")),
		"",
	}
}

func (m *monitorModel) footer() string {
	s := m.styles

	if m.stopping {
		return s.bad.Render("stopping…") + s.faint.Render("  press q again to detach")
	}

	keys := s.faint.Render("q") + s.dim.Render(" stop") +
		s.faint.Render("  ·  tab") + s.dim.Render(" switch")

	if m.tab == tabOutput {
		keys += s.faint.Render("  ·  ↑↓/pgup") + s.dim.Render(" scroll") +
			s.faint.Render("  ·  g") + s.dim.Render(" follow")
	}

	return keys
}

//nolint:funlen // one screen of gauges, kept together
func (m *monitorModel) runtimeTab(inner int) []string {
	s, sn := m.styles, m.sn

	sparkW := clamp(inner/3, 12, 40)

	gauge := func(label, value string, ser series, unit string) string {
		line := s.dim.Render(padRight(label, 12)) + s.title.Render(padLeft(value, 10)) + " " + s.faint.Render(unit)
		if len(ser.values) > 1 {
			line = twoCol(line, s.accent.Render(floatSparkline(ser.values, sparkW)), inner-4)
		}

		return line
	}

	memory := []string{
		gauge("rss", fmt.Sprintf("%.1f", sn.rss.last), sn.rss, "MB"),
		gauge("heap live", fmt.Sprintf("%.1f", sn.heap.last), sn.heap, "MB"),
		gauge("allocating", fmt.Sprintf("%.1f", sn.alloc.last), sn.alloc, "MB/s"),
	}

	if len(sn.gcs) > 0 {
		last := sn.gcs[len(sn.gcs)-1]
		memory = append(memory, s.dim.Render(padRight("next gc", 12))+
			s.title.Render(padLeft(fmt.Sprintf("%.1f", last.goal), 10))+" "+s.faint.Render("MB"))
	}

	// A heap that climbs while live data climbs with it is the shape of a leak.
	if note := leakNote(s, sn); note != "" {
		memory = append(memory, note)
	}

	out := s.panel(s.section.Render("MEMORY"), s.faint.Render(gcSummary(sn)), memory, inner, 5)

	runtimeRows := []string{
		gauge("cpu", fmt.Sprintf("%.1f", sn.cpu.last), sn.cpu, "% of a core"),
		goroutineRow(s, sn, inner-4),
	}

	if sn.sched.procs > 0 {
		runtimeRows = append(runtimeRows,
			s.dim.Render(padRight("threads", 12))+s.title.Render(padLeft(fmt.Sprint(sn.sched.threads), 10))+
				s.faint.Render(fmt.Sprintf("   gomaxprocs %d · idle %d · runqueue %d",
					sn.sched.procs, sn.sched.idle, sn.sched.runqueue)))
	}

	if note := goroutineLeakNote(s, sn); note != "" {
		runtimeRows = append(runtimeRows, note)
	}

	out = append(out, s.panel(s.section.Render("CPU & SCHEDULER"),
		s.faint.Render(waitSummary(sn)), runtimeRows, inner, 4)...)

	if verdict := gcVerdict(s, sn, inner-4); len(verdict) > 0 {
		out = append(out, s.panel(s.section.Render("GC PRESSURE"), "", verdict, inner, len(verdict))...)
	}

	if startup := startupRows(s, sn, inner-4); len(startup) > 0 {
		out = append(out, s.panel(
			s.section.Render("STARTUP")+s.faint.Render(" · package init, slowest first"),
			s.faint.Render(startupSummary(sn)), startup, inner, len(startup))...)
	}

	rows := clamp(m.height-len(out)-7, 3, 10)
	gcRows := make([]string, 0, rows)

	for i := len(sn.gcs) - 1; i >= 0 && len(gcRows) < rows; i-- {
		ev := sn.gcs[i]
		gcRows = append(gcRows, s.faint.Render(padLeft("#"+fmt.Sprint(ev.num), 6))+
			s.dim.Render(padLeft(fmtDur(ev.at), 9))+"  "+
			s.title.Render(fmt.Sprintf("%.1f→%.1f MB", ev.heapPrev, ev.heapLive))+
			s.faint.Render(fmt.Sprintf("   goal %.1f MB", ev.goal))+
			"   "+s.durStyle(ev.pause).Render("pause "+fmtDur(ev.pause)))
	}

	if len(gcRows) == 0 {
		gcRows = append(gcRows, s.faint.Render("no garbage collection yet"))
	}

	return append(out, s.panel(s.section.Render("GARBAGE COLLECTION"),
		s.faint.Render("newest first"), gcRows, inner, rows)...)
}

func (m *monitorModel) outputTab(inner int) []string {
	s, sn := m.styles, m.sn

	rows := max(m.height-6, 3)
	total := len(sn.output)

	end := total - m.scroll
	end = clamp(end, 0, total)
	start := max(end-rows, 0)

	lines := make([]string, 0, rows)
	for _, l := range sn.output[start:end] {
		lines = append(lines, s.dim.Render(truncateRight(l, inner-4)))
	}

	for len(lines) < rows {
		lines = append(lines, "")
	}

	position := s.faint.Render(fmt.Sprintf("%d lines", total))
	if !m.follow {
		position = s.warn.Render(fmt.Sprintf("paused · %d lines below", m.scroll))
	}

	return s.panel(s.section.Render("PROGRAM OUTPUT"), position, lines, inner, rows)
}

func (m *monitorModel) profileTab(inner int) []string {
	s, sn := m.styles, m.sn

	if sn.pprofAddr == "" {
		return s.panel(s.section.Render("PROFILE"), "", []string{
			s.faint.Render("no pprof endpoint given."),
			"",
			s.dim.Render("Import net/http/pprof in the program and start a listener,"),
			s.dim.Render("then re-run with  -pprof=localhost:6060"),
			"",
			s.faint.Render("Memory, CPU and GC on the runtime tab need nothing from the program."),
		}, inner, 6)
	}

	rows := max((m.height-12)/3, 3)

	cpu := strings.Split(strings.TrimRight(sn.cpuProfile, "\n"), "\n")
	heap := strings.Split(strings.TrimRight(sn.heapProfile, "\n"), "\n")

	note := s.faint.Render(sn.pprofAddr)
	if sn.pprofNote != "" {
		note = s.bad.Render(sn.pprofNote)
	}

	out := s.panel(s.section.Render("CPU")+s.faint.Render(" · 5 second sample"), note,
		profileRows(s, cpu, rows, inner), inner, rows)

	out = append(out, s.panel(s.section.Render("HEAP")+s.faint.Render(" · in use"), "",
		profileRows(s, heap, rows, inner), inner, rows)...)

	stacks := make([]string, 0, rows)
	for _, g := range sn.stacks {
		if len(stacks) == rows {
			break
		}
		stacks = append(stacks, s.title.Render(padLeft(fmt.Sprint(g.count), 6))+"  "+
			s.dim.Render(truncateRight(g.where, inner-12)))
	}

	if len(stacks) == 0 {
		stacks = append(stacks, s.faint.Render("waiting for the first goroutine profile…"))
	}

	return append(out, s.panel(
		s.section.Render("GOROUTINES")+s.faint.Render(" · grouped by where they are blocked"),
		s.faint.Render(fmt.Sprintf("%d total", sn.goroutines)), stacks, inner, rows)...)
}

// profileRows trims the header `go tool pprof -top` prints before the table.
func profileRows(s *styles, lines []string, rows, inner int) []string {
	out := make([]string, 0, rows)

	for _, l := range lines {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "File:") ||
			strings.HasPrefix(l, "Build ID:") || strings.HasPrefix(l, "Type:") ||
			strings.HasPrefix(l, "Time:") || strings.HasPrefix(l, "Duration:") {
			continue
		}

		out = append(out, s.dim.Render(truncateRight(strings.TrimSpace(l), inner-4)))
		if len(out) == rows {
			break
		}
	}

	if len(out) == 0 {
		out = append(out, s.faint.Render("waiting for the first sample…"))
	}

	return out
}

func goroutineRow(s *styles, sn monitorSnap, width int) string {
	if sn.goroutines == 0 {
		return s.dim.Render(padRight("goroutines", 12)) +
			s.faint.Render("       — needs -pprof or -goroutines")
	}

	row := s.dim.Render(padRight("goroutines", 12)) + s.title.Render(padLeft(fmt.Sprint(sn.goroutines), 10))
	if len(sn.goroutineTrend.values) > 1 {
		row = twoCol(row, s.accent.Render(floatSparkline(sn.goroutineTrend.values, clamp(width/3, 12, 40))), width)
	}

	return row
}

// waitSummary names what the goroutines are waiting on, which is the difference
// between a busy program and a stuck one.
func waitSummary(sn monitorSnap) string {
	if len(sn.waitReasons) == 0 {
		return ""
	}

	type entry struct {
		reason string
		n      int
	}

	all := make([]entry, 0, len(sn.waitReasons))
	for reason, n := range sn.waitReasons {
		all = append(all, entry{reason, n})
	}

	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })

	parts := make([]string, 0, 3)
	for i, e := range all {
		if i == 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%d %s", e.n, e.reason))
	}

	return strings.Join(parts, " · ")
}

// goroutineLeakNote speaks up only for growth that has kept going: a service
// that opens a few goroutines per request is not leaking, one whose count only
// ever rises is.
func goroutineLeakNote(s *styles, sn monitorSnap) string {
	values := sn.goroutineTrend.values
	if len(values) < 8 {
		return ""
	}

	first, last := values[0], values[len(values)-1]
	if first <= 0 || last < first*2 || last-first < 20 {
		return ""
	}

	// Growth that never gives any back is what separates a leak from load.
	for i := 1; i < len(values); i++ {
		if values[i] < values[i-1]*0.8 {
			return ""
		}
	}

	note := s.warn.Render("⚠ goroutines only grow ") +
		s.title.Render(fmt.Sprintf("%.0f → %.0f", first, last))

	if len(sn.stacks) > 0 {
		note += s.faint.Render("  most in ") + s.dim.Render(truncateRight(sn.stacks[0].where, 40))
	}

	return note
}

// gcVerdict turns the three numbers we have into advice. It is a heuristic and
// says so: doubling GOGC roughly halves the collector's share of the cpu, at
// roughly twice the live heap.
func gcVerdict(s *styles, sn monitorSnap, width int) []string {
	if len(sn.gcs) < 3 {
		return nil
	}

	last := sn.gcs[len(sn.gcs)-1]

	rate := 0.0
	if window := last.at - sn.gcs[0].at; window > 0 {
		rate = float64(len(sn.gcs)-1) / window.Minutes()
	}

	facts := s.dim.Render(fmt.Sprintf("%.1f%% of cpu in gc", last.gcCPU)) +
		s.faint.Render(" · ") + s.dim.Render(fmt.Sprintf("%.1f MB/s allocated", sn.alloc.last)) +
		s.faint.Render(" · ") + s.dim.Render(fmt.Sprintf("%.0f collections/min", rate))

	out := []string{facts}

	if env := gcEnvNote(s); env != "" {
		out = append(out, env)
	}

	if last.gcCPU >= 10 {
		out = append(out, s.warn.Render("→ ")+s.dim.Render(fmt.Sprintf(
			"GOGC=%d would roughly halve that, at about twice the live heap", currentGOGC()*2)))
	}

	return out
}

func gcEnvNote(s *styles) string {
	parts := []string{s.faint.Render(fmt.Sprintf("GOGC=%d", currentGOGC()))}

	if limit := os.Getenv("GOMEMLIMIT"); limit != "" {
		parts = append(parts, s.faint.Render("GOMEMLIMIT="+limit))
	} else {
		parts = append(parts, s.faint.Render("GOMEMLIMIT unset"))
	}

	return strings.Join(parts, s.faint.Render(" · "))
}

func currentGOGC() int {
	if v, err := strconv.Atoi(os.Getenv("GOGC")); err == nil && v > 0 {
		return v
	}

	return 100 // the runtime default
}

// startupRows shows where the time before main() went.
func startupRows(s *styles, sn monitorSnap, width int) []string {
	if len(sn.inits) == 0 {
		return nil
	}

	sorted := append([]initEvent(nil), sn.inits...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].clock > sorted[j].clock })

	rows := make([]string, 0, 4)

	for _, ev := range sorted {
		if ev.clock <= 0 || len(rows) == 4 {
			break
		}

		rows = append(rows, s.durStyle(ev.clock).Render(padLeft(fmtDur(ev.clock), 8))+"  "+
			s.dim.Render(truncate(ev.pkg, width-30))+
			s.faint.Render(fmt.Sprintf("   %s, %d allocs", humanBytes(ev.bytes), ev.allocs)))
	}

	return rows
}

func startupSummary(sn monitorSnap) string {
	var total time.Duration
	for _, ev := range sn.inits {
		total += ev.clock
	}

	return fmt.Sprintf("%s in %d packages", fmtDur(total), len(sn.inits))
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func gcSummary(sn monitorSnap) string {
	if len(sn.gcs) == 0 {
		return ""
	}

	last := sn.gcs[len(sn.gcs)-1]

	return fmt.Sprintf("%d collections · %.1f%% of cpu in gc", last.num, last.gcCPU)
}

// leakNote is deliberately cautious: it only speaks up when the live heap has
// grown across a good number of collections, which is what a leak looks like
// from the outside.
func leakNote(s *styles, sn monitorSnap) string {
	if len(sn.gcs) < 10 {
		return ""
	}

	first := sn.gcs[0].heapLive
	last := sn.gcs[len(sn.gcs)-1].heapLive

	if first <= 0 || last < first*2 || last-first < 8 {
		return ""
	}

	return s.warn.Render("⚠ live heap grew ") +
		s.title.Render(fmt.Sprintf("%.1f → %.1f MB", first, last)) +
		s.faint.Render(fmt.Sprintf(" over %d collections", len(sn.gcs)))
}

// monitorSummary is what stays on the terminal once the program is gone.
func monitorSummary(s *styles, sn monitorSnap) string {
	var out []string

	add := func(l string) { out = append(out, l) }

	add("")

	if sn.exitErr != "" {
		add(gutter + s.bad.Render("✗ program exited") + s.dim.Render(" — "+sn.exitErr) +
			s.faint.Render(" after "+fmtDur(sn.uptime)))
	} else {
		add(gutter + s.ok.Render("✓ program exited") + s.dim.Render(" after ") + s.title.Render(fmtDur(sn.uptime)))
	}

	facts := []string{
		s.dim.Render(fmt.Sprintf("peak rss %.1f MB", sn.rss.peak())),
		s.dim.Render(fmt.Sprintf("peak cpu %.0f%%", sn.cpu.peak())),
	}

	if len(sn.gcs) > 0 {
		last := sn.gcs[len(sn.gcs)-1]
		facts = append(facts,
			s.dim.Render(fmt.Sprintf("%d collections", last.num)),
			s.dim.Render(fmt.Sprintf("%.1f%% cpu in gc", last.gcCPU)),
			s.dim.Render(fmt.Sprintf("peak live heap %.1f MB", sn.heap.peak())),
		)
	}

	add(gutter + "  " + strings.Join(facts, s.faint.Render(" · ")))

	if note := leakNote(s, sn); note != "" {
		add("")
		add(gutter + note)
	}

	if tail := lastOutput(sn.output, 15); len(tail) > 0 {
		add("")
		add(gutter + s.section.Render("LAST OUTPUT"))

		for _, l := range tail {
			add(gutter + s.faint.Render("│ ") + l)
		}
	}

	return strings.Join(out, "\n") + "\n\n"
}

func lastOutput(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}

	return lines
}

// runMonitorUI owns the terminal while the program runs.
func runMonitorUI(st *monitorState, stop func()) {
	teaOpts := []tea.ProgramOption{tea.WithOutput(os.Stderr), tea.WithoutSignalHandler()}
	if term.IsTerminal(os.Stdin.Fd()) {
		teaOpts = append(teaOpts, tea.WithInput(os.Stdin))
	} else {
		teaOpts = append(teaOpts, tea.WithInput(nil))
	}

	prog := tea.NewProgram(newMonitorModel(st, stop), teaOpts...)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "shiny-goggles: monitor UI unavailable: %v\n", err)
	}
}

// runMonitorPlain is the fallback when no terminal is attached: the program's
// output goes straight through and the runtime numbers are printed now and then.
func runMonitorPlain(ctx context.Context, st *monitorState) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	printed := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sn := st.snapshot()
			for _, line := range sn.output[min(printed, len(sn.output)):] {
				fmt.Println(line)
			}
			printed = len(sn.output)

			fmt.Fprintf(os.Stderr, "  [%s] rss %.1f MB · heap %.1f MB · cpu %.1f%% · %d gc\n",
				fmtDur(sn.uptime), sn.rss.last, sn.heap.last, sn.cpu.last, len(sn.gcs))

			if sn.exited {
				return
			}
		}
	}
}
