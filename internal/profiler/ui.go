package profiler

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// uiTick drives both the animation and the parallelism sampling, so the
// sparkline resolution is exactly one cell per tick.
const uiTick = 100 * time.Millisecond

type (
	finishMsg struct{}
	quitMsg   struct{}
	tickMsg   time.Time
)

type model struct {
	st        *stats
	opts      *options
	eta       time.Duration
	title     string
	interrupt func()
	styles    *styles

	width  int
	height int
	frame  int
	sized  bool // the terminal told us its real size

	sn       snap
	showStd  bool
	done     bool
	stopping bool
}

func newModel(st *stats, opts *options, eta time.Duration, goArgs []string, interrupt func()) *model {
	return &model{
		st:        st,
		opts:      opts,
		eta:       eta,
		title:     strings.Join(goArgs, " "),
		interrupt: interrupt,
		styles:    newStyles(),
		showStd:   !opts.hideStd,
		width:     terminalWidth(),
		height:    24,
		sn:        st.snapshot(),
	}
}

func (m *model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(uiTick, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A pty with no size reports zeros; keep the fallback in that case.
		if msg.Width > 0 {
			m.width, m.sized = msg.Width, true
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}

		return m, nil

	case tickMsg:
		if m.done {
			return m, nil
		}
		m.frame++
		m.st.sample()
		m.sn = m.st.snapshot()

		return m, tick()

	case finishMsg:
		m.done = true
		m.sn = m.st.snapshot()

		// One more frame so the final screen is what stays behind, then quit.
		return m, tea.Tick(uiTick/2, func(time.Time) tea.Msg { return quitMsg{} })

	case quitMsg:
		return m, tea.Quit

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	return m, nil
}

func (m *model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		if m.stopping {
			// Second press: stop waiting for the go command to wind down.
			return m, tea.Quit
		}
		m.stopping = true
		if m.interrupt != nil {
			m.interrupt()
		}

		return m, nil

	case "v":
		m.showStd = !m.showStd

		return m, nil
	}

	return m, nil
}

func (m *model) View() string {
	var view string

	switch {
	case m.done && m.opts.clear:
		// Asked to get out of the way: an empty frame makes the renderer shrink
		// from the whole screen to a single line and erase everything below it.
		return ""

	case m.opts.verbose():
		// Otherwise the screen is kept: on a three minute build it is the only
		// place where the timeline and the charts can still be read afterwards.
		// bubbletea erases the last line of the final frame, so leave it spare.
		view = m.renderLive()
		if m.done {
			view += "\n"
		}

	case m.done:
		// The preloader has nothing left to say; the one line report the driver
		// prints below takes its place.
		view = "\n"

	default:
		view = m.renderSimple()
	}

	if m.sized {
		return view
	}

	// Without a reported terminal size bubbletea cannot erase to end of line,
	// so previous frames would shine through. Overwrite them with our own
	// padding instead.
	lines := strings.Split(view, "\n")
	for i, l := range lines {
		if pad := m.width - lipgloss.Width(l); pad > 0 {
			lines[i] = l + strings.Repeat(" ", pad)
		}
	}

	return strings.Join(lines, "\n")
}
