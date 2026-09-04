package profiler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestNormalizeGoCommand(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		cases := map[string]struct {
			in   []string
			want []string
		}{
			"a bare subcommand gets go":      {[]string{"build", "./..."}, []string{"go", "build", "./..."}},
			"explicit go is kept":            {[]string{"go", "run", "."}, []string{"go", "run", "."}},
			"a path to go is kept":           {[]string{"/opt/go1.25/bin/go", "test"}, []string{"/opt/go1.25/bin/go", "test"}},
			"a versioned wrapper is kept":    {[]string{"go1.24.3", "build", "."}, []string{"go1.24.3", "build", "."}},
			"a path to a wrapper is kept":    {[]string{"/Users/x/go/bin/go1.24.3", "build"}, []string{"/Users/x/go/bin/go1.24.3", "build"}},
			"go.exe is kept":                 {[]string{"go.exe", "build"}, []string{"go.exe", "build"}},
			"a subcommand we cannot profile": {[]string{"mod", "tidy"}, []string{"go", "mod", "tidy"}},
		}

		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				got, err := normalizeGoCommand(tc.in)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Join(got, " ") != strings.Join(tc.want, " ") {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			})
		}
	})

	// Running something unprofiled with no screen and no explanation is the
	// most confusing thing this could do, so it refuses instead.
	t.Run("refused", func(t *testing.T) {
		t.Parallel()

		for _, argv := range [][]string{
			{"make", "server"},
			{"gofmt", "-l", "."},
			{"golangci-lint", "run"},
			{},
		} {
			if _, err := normalizeGoCommand(argv); err == nil {
				t.Fatalf("%v should have been refused", argv)
			}
		}
	})
}

func TestInjectToolexec(t *testing.T) {
	t.Parallel()

	t.Run("flags land after the subcommand", func(t *testing.T) {
		t.Parallel()

		got, sub, ok := injectToolexec([]string{"go", "run", ".", "serve"}, "/bin/gbp", &options{})
		if !ok || sub != "run" {
			t.Fatalf("ok=%v sub=%q", ok, sub)
		}

		want := "go run -toolexec=/bin/gbp _toolexec . serve"
		if strings.Join(got, " ") != want {
			t.Fatalf("got %q, want %q", strings.Join(got, " "), want)
		}
	})

	t.Run("only the go command's own debug flags are passed through", func(t *testing.T) {
		t.Parallel()

		got, _, ok := injectToolexec(
			[]string{"go", "build", "./..."}, "/bin/gbp",
			&options{trace: "tmp/mine.json", goTrace: "tmp/go.json", actiongraph: "tmp/g.json"},
		)
		if !ok {
			t.Fatal("expected instrumentation")
		}

		joined := strings.Join(got, " ")
		if !strings.Contains(joined, "-debug-trace=tmp/go.json") ||
			!strings.Contains(joined, "-debug-actiongraph=tmp/g.json") {
			t.Fatalf("missing debug flags in %q", joined)
		}
		// -trace is written by us afterwards; the go command must not see it.
		if strings.Contains(joined, "mine.json") {
			t.Fatalf("our own trace leaked into the go command line: %q", joined)
		}
	})

	t.Run("unsupported subcommand is left alone", func(t *testing.T) {
		t.Parallel()

		if _, _, ok := injectToolexec([]string{"go", "mod", "tidy"}, "/bin/gbp", &options{}); ok {
			t.Fatal("go mod tidy must not be instrumented")
		}
	})

	t.Run("existing toolexec wins", func(t *testing.T) {
		t.Parallel()

		if _, _, ok := injectToolexec([]string{"go", "build", "-toolexec=other"}, "/bin/gbp", &options{}); ok {
			t.Fatal("must not override a user supplied -toolexec")
		}
	})

	t.Run("paths with spaces are quoted", func(t *testing.T) {
		t.Parallel()

		got, _, _ := injectToolexec([]string{"go", "build", "."}, "/opt/my tools/gbp", &options{})
		if !strings.Contains(strings.Join(got, " "), `-toolexec="/opt/my tools/gbp" _toolexec`) {
			t.Fatalf("path not quoted: %v", got)
		}
	})
}

func TestToolexecArgs(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}

	t.Run("explicit marker", func(t *testing.T) {
		t.Parallel()

		args, ok := toolexecArgs([]string{"gbp", toolexecMarker, "/tool/compile", "-o", "x.a"})
		if !ok || args[0] != "/tool/compile" {
			t.Fatalf("ok=%v args=%v", ok, args)
		}
	})

	t.Run("legacy bare wrapper", func(t *testing.T) {
		t.Parallel()

		args, ok := toolexecArgs([]string{"gbp", self, "-V=full"})
		if !ok || args[0] != self {
			t.Fatalf("ok=%v args=%v", ok, args)
		}
	})

	t.Run("driver invocation", func(t *testing.T) {
		t.Parallel()

		if _, ok := toolexecArgs([]string{"gbp", "go", "build", "./..."}); ok {
			t.Fatal("driver invocation must not be treated as a tool wrapper")
		}
	})
}

func TestIsStdBuild(t *testing.T) {
	t.Parallel()

	if !isStdBuild([]string{"-o", "x.a", "-std", "-p", "net/http"}) {
		t.Fatal("expected std")
	}
	if isStdBuild([]string{"-o", "x.a", "-p", "example.com/x"}) {
		t.Fatal("expected non std")
	}
}

// The go command runs `asm -gensymabis` before compiling a package, so the very
// first action of a stdlib package arrives without the -std marker.
func TestStatsResolvesStdlibAfterTheFact(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)

	st.start(startEvent{id: 1, tool: "asm", pkg: "internal/cpu"})
	st.end(endEvent{id: 1, dur: 30 * time.Millisecond})
	st.start(startEvent{id: 2, tool: "compile", pkg: "internal/cpu", std: true})
	st.end(endEvent{id: 2, dur: 40 * time.Millisecond})
	st.start(startEvent{id: 3, tool: "compile", pkg: "example.com/app"})
	st.end(endEvent{id: 3, dur: 50 * time.Millisecond})

	sn := st.snapshot()

	if sn.compiled != 1 || sn.std != 1 {
		t.Fatalf("compiled=%d std=%d, want 1/1", sn.compiled, sn.std)
	}
	if got := len(sn.visibleSlow(false, 10)); got != 1 {
		t.Fatalf("non-std chart has %d entries, want only example.com/app", got)
	}
	if got := len(sn.visibleSlow(true, 10)); got != 3 {
		t.Fatalf("full chart has %d entries, want 3", got)
	}
}

func TestStatsKeepsSlowestSorted(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)
	for i, d := range []time.Duration{10, 90, 30, 70, 50} {
		//nolint:gosec // small loop index
		id := uint64(i + 1)
		st.start(startEvent{id: id, tool: "compile", pkg: "p"})
		st.end(endEvent{id: id, dur: d * time.Millisecond})
	}

	slow := st.snapshot().visibleSlow(true, 5)
	for i := 1; i < len(slow); i++ {
		if slow[i-1].dur < slow[i].dur {
			t.Fatalf("not sorted: %v", slow)
		}
	}
	if slow[0].dur != 90*time.Millisecond {
		t.Fatalf("head is %v, want 90ms", slow[0].dur)
	}
}

func TestStatsAbortedActionLeavesRunningList(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)
	st.start(startEvent{id: 1, tool: "compile", pkg: "p"})
	st.end(endEvent{id: 1, aborted: true})

	sn := st.snapshot()
	if len(sn.active) != 0 || sn.aborted != 1 {
		t.Fatalf("active=%d aborted=%d", len(sn.active), sn.aborted)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	if got := truncate("github.com/foo/bar/baz", 12); got != "…/bar/baz" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("short", 12); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := truncateRight("go build -o /tmp/x ./main.go", 10); got != "go build …" {
		t.Fatalf("got %q", got)
	}
}

func TestSparkline(t *testing.T) {
	t.Parallel()

	if got := sparkline(nil, 10); got != "" {
		t.Fatalf("got %q for no samples", got)
	}

	got := []rune(sparkline([]uint16{0, 4, 8}, 3))
	if len(got) != 3 {
		t.Fatalf("got %d cells, want 3", len(got))
	}
	if got[0] != '▁' || got[2] != '█' {
		t.Fatalf("got %q, want a rising line", string(got))
	}
}

func TestHistoryETAUsesMedian(t *testing.T) {
	t.Parallel()

	h := &history{Commands: map[string][]float64{"go build .": {10, 2, 4}}}
	if got := h.eta("go build ."); got != 4*time.Second {
		t.Fatalf("got %v, want 4s", got)
	}
	if got := h.eta("go test ./..."); got != 0 {
		t.Fatalf("got %v for an unknown command, want 0", got)
	}
}

// The layout must not depend on the data, otherwise panels resize while the
// build fills them in and the whole screen jumps.
func TestLayoutReservesFixedHeights(t *testing.T) {
	t.Parallel()

	t.Run("a tall terminal gets everything", func(t *testing.T) {
		t.Parallel()

		l := newLayout(60, defaultSlowRows, defaultRecentRows, defaultBlockingRows)
		if l.slow != defaultSlowRows || l.run != fullRunRows ||
			l.timeline != fullTimelineRows || l.recent != defaultRecentRows ||
			l.blocking != defaultBlockingRows {
			t.Fatalf("got %+v", l)
		}
	})

	t.Run("shrinking keeps the slowest chart last", func(t *testing.T) {
		t.Parallel()

		for h := 12; h <= 50; h++ {
			l := newLayout(h, defaultSlowRows, defaultRecentRows, defaultBlockingRows)

			used := 4
			for _, rows := range []int{l.run, l.blocking, l.slow, l.timeline, l.recent} {
				if rows < 0 {
					t.Fatalf("height %d produced negative rows: %+v", h, l)
				}
				if rows > 0 {
					used += rows + 2
				}
			}

			if used > max(h-1, 8) {
				t.Fatalf("height %d: layout %+v needs %d lines", h, l, used)
			}
			if l.recent > 0 && l.slow < minSlowRows {
				t.Fatalf("height %d: dropped the chart before the done list: %+v", h, l)
			}
		}
	})
}

// Solo time is what makes a package a bottleneck: a slow package that compiled
// next to seven others cost the build almost nothing, a slow package that
// compiled on its own cost the build its whole duration.
func TestSoloTimeIsOnlyChargedWhenAlone(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)

	// "alone" runs by itself for a while, then "friend" joins it.
	st.start(startEvent{id: 1, tool: "compile", pkg: "alone"})
	time.Sleep(150 * time.Millisecond)
	st.start(startEvent{id: 2, tool: "compile", pkg: "friend"})
	time.Sleep(50 * time.Millisecond)
	st.end(endEvent{id: 1, dur: time.Second})
	st.end(endEvent{id: 2, dur: time.Second})

	sn := st.snapshot()

	blocked := sn.visibleBlocking(10)
	if len(blocked) != 1 {
		t.Fatalf("got %d blocking actions, want only \"alone\": %+v", len(blocked), blocked)
	}
	if blocked[0].pkg != "alone" {
		t.Fatalf("blamed %q", blocked[0].pkg)
	}
	// Only a lower bound: a loaded machine stretches the sleeps, and the point
	// is that the time was charged at all, not that the scheduler was punctual.
	if blocked[0].solo < 150*time.Millisecond {
		t.Fatalf("solo=%v, want at least the 150ms it spent alone", blocked[0].solo)
	}
	if blocked[0].solo > time.Second {
		t.Fatalf("solo=%v, far beyond the window it was actually alone", blocked[0].solo)
	}

	// The concurrent stretch counts once, not once per action.
	if sn.serial < 150*time.Millisecond || sn.serial > time.Second {
		t.Fatalf("serial=%v, want roughly the 150ms one action ran alone", sn.serial)
	}
	if share := sn.serialShare(); share <= 0 || share > 1 {
		t.Fatalf("serialShare=%v", share)
	}
}

func TestSoloTimeCountsGapsWithNothingRunningAsIdle(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)

	time.Sleep(120 * time.Millisecond) // the go command loading packages
	st.start(startEvent{id: 1, tool: "compile", pkg: "p"})
	st.end(endEvent{id: 1, dur: time.Millisecond})

	sn := st.snapshot()
	if sn.idle < 120*time.Millisecond {
		t.Fatalf("idle=%v, want at least the 120ms nothing was running", sn.idle)
	}
	if len(sn.blocking) != 0 {
		t.Fatalf("a 1ms action must not be called blocking: %+v", sn.blocking)
	}
}

// A package blocking the build right now must be in the table already, not only
// once it finishes — that is the whole point of watching the screen.
func TestBlockingIncludesTheActionInFlight(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)
	st.start(startEvent{id: 1, tool: "compile", pkg: "still/running"})
	time.Sleep(150 * time.Millisecond)

	sn := st.snapshot()
	if len(sn.blocking) != 1 || sn.blocking[0].pkg != "still/running" {
		t.Fatalf("got %+v", sn.blocking)
	}
	if !sn.active[0].blocking() {
		t.Fatalf("the running action is not marked: solo=%v", sn.active[0].solo)
	}
}

func TestCompileTarget(t *testing.T) {
	t.Parallel()

	line := "/usr/local/go/pkg/tool/linux_amd64/compile -o $WORK/b007/_pkg_.a -trimpath x " +
		"-p internal/goarch -lang=go1.26 -std -pack a.go\n"

	if pkg, ok := compileTarget(line); !ok || pkg != "internal/goarch" {
		t.Fatalf("got %q ok=%v", pkg, ok)
	}

	for _, other := range []string{
		"/usr/local/go/pkg/tool/linux_amd64/asm -p internal/abi -o x.o a.s\n",
		"go tool buildid -w $WORK/b006/_pkg_.a # internal\n",
		"mkdir -p $WORK/b007/\n",
		"",
	} {
		if _, ok := compileTarget(other); ok {
			t.Fatalf("%q must not count as a compile action", other)
		}
	}
}

func TestScanCompileLinesCountsPackages(t *testing.T) {
	t.Parallel()

	out := "mkdir -p $WORK/b1/\n" +
		"/go/pkg/tool/linux_amd64/compile -o a.a -p fmt -std x.go\n" +
		"/go/pkg/tool/linux_amd64/asm -p fmt -o a.o a.s\n" +
		"/go/pkg/tool/linux_amd64/compile -o b.a -p net/http x.go\n" +
		"/go/pkg/tool/linux_amd64/compile -o c.a -p fmt y.go\n" // same package twice

	got := scanCompileLines(strings.NewReader(out))
	if len(got) != 2 || !got["fmt"] || !got["net/http"] {
		t.Fatalf("got %v", got)
	}
}

// The denominator is the union of the plan and what we have already watched, so
// packages the build finished before the plan was produced still count once.
func TestProgressUsesUnionOfPlanAndObserved(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)

	st.start(startEvent{id: 1, tool: "compile", pkg: "already/done"})
	st.end(endEvent{id: 1, dur: time.Millisecond})

	if _, known := st.snapshot().progress(); known {
		t.Fatal("no plan yet, progress must be unknown")
	}

	// The plan misses already/done: the build had cached it in the meantime.
	st.addPlan(map[string]bool{"a": true, "b": true, "c": true})

	sn := st.snapshot()
	if sn.planned != 4 {
		t.Fatalf("planned=%d, want 4", sn.planned)
	}

	frac, known := sn.progress()
	if !known || frac != 0.25 {
		t.Fatalf("frac=%v known=%v, want 0.25", frac, known)
	}
}

func TestTimelineRowsAreFixedAndPacked(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)
	for i := range 4 { // four overlapping actions must occupy four lanes
		//nolint:gosec // small loop index
		id := uint64(i + 1)
		st.start(startEvent{id: id, tool: "compile", pkg: fmt.Sprintf("pkg/number%d", i)})
	}
	for i := range 4 {
		//nolint:gosec // small loop index
		st.end(endEvent{id: uint64(i + 1), dur: 500 * time.Millisecond})
	}

	// Pin the window so the four overlapping slices land in the middle of it.
	sn := st.snapshot()
	sn.startedAt = sn.timeline[0].start
	sn.elapsed = time.Second

	rows := timelineRows(newStyles(), sn, 60, 6)
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want exactly 6", len(rows))
	}

	painted := 0
	for _, r := range rows {
		if strings.TrimSpace(stripANSI(r)) != "" {
			painted++
		}
	}
	if painted != 4 {
		t.Fatalf("%d lanes carry a slice, want 4", painted)
	}
}

func TestAssignLanesPacksOverlappingWork(t *testing.T) {
	t.Parallel()

	base := time.Now()
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	items := []interval{
		{start: at(0), end: at(100)},   // ─────
		{start: at(10), end: at(90)},   //  ────   overlaps the first
		{start: at(20), end: at(30)},   //   ──    overlaps both
		{start: at(200), end: at(300)}, /*        starts after all of them */ //nolint:gofmt // aligned comments
	}

	t.Run("unbounded gives every overlap its own lane", func(t *testing.T) {
		t.Parallel()

		got := assignLanes(items, 0)
		if want := []int{0, 1, 2, 0}; !slicesEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("a limit folds the overflow instead of dropping it", func(t *testing.T) {
		t.Parallel()

		got := assignLanes(items, 2)
		if len(got) != len(items) {
			t.Fatalf("got %d lanes for %d items", len(got), len(items))
		}
		for i, lane := range got {
			if lane < 0 || lane > 1 {
				t.Fatalf("item %d landed on lane %d, outside the limit", i, lane)
			}
		}
	})
}

// perfetto rejected the go command's own trace over flow events that do not sit
// inside a slice, so ours must have none — and no overlapping slices on a track.
func TestWriteTraceIsCleanForPerfetto(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, nil)
	for i := range 6 {
		//nolint:gosec // small loop index
		id := uint64(i + 1)
		st.start(startEvent{id: id, tool: "compile", pkg: fmt.Sprintf("example.com/p%d", i)})
	}
	for i := range 6 {
		//nolint:gosec // small loop index
		st.end(endEvent{id: uint64(i + 1), dur: time.Duration(i+1) * 10 * time.Millisecond})
	}
	st.finish()

	path := filepath.Join(t.TempDir(), "trace.json")
	if err := writeTrace(path, "go build ./...", st.snapshot()); err != nil {
		t.Fatal(err)
	}

	var file struct {
		DisplayTimeUnit string `json:"displayTimeUnit"`
		Events          []struct {
			Name string         `json:"name"`
			Cat  string         `json:"cat"`
			Ph   string         `json:"ph"`
			Ts   int64          `json:"ts"`
			Dur  int64          `json:"dur"`
			Tid  int            `json:"tid"`
			Args map[string]any `json:"args"`
		} `json:"traceEvents"`
	}

	raw, err := os.ReadFile(path) //nolint:gosec // path is from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("perfetto would not parse this: %v", err)
	}

	if file.DisplayTimeUnit != "ms" {
		t.Fatalf("displayTimeUnit=%q", file.DisplayTimeUnit)
	}

	type span struct{ from, to int64 }

	byTrack := map[int][]span{}
	slices, workers := 0, 0

	for _, e := range file.Events {
		switch e.Ph {
		case "s", "f", "t":
			t.Fatalf("flow event %q would be reported as FLOW_NO_ENCLOSING_SLICE", e.Name)
		case "X":
			if e.Ts < 0 || e.Dur <= 0 {
				t.Fatalf("slice %q has ts=%d dur=%d", e.Name, e.Ts, e.Dur)
			}
			if e.Cat != "build" {
				slices++
			}
			byTrack[e.Tid] = append(byTrack[e.Tid], span{e.Ts, e.Ts + e.Dur})
		case "M":
			if e.Name == "thread_name" {
				workers++
			}
		}
	}

	if slices != 6 {
		t.Fatalf("got %d action slices, want 6", slices)
	}
	if workers != 6 {
		t.Fatalf("got %d worker tracks for 6 overlapping actions", workers)
	}

	for tid, s := range byTrack {
		sort.Slice(s, func(i, j int) bool { return s[i].from < s[j].from })
		for i := 1; i < len(s); i++ {
			if s[i].from < s[i-1].to {
				t.Fatalf("track %d has overlapping slices: %+v and %+v", tid, s[i-1], s[i])
			}
		}
	}
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestSliceLabelKeepsExactWidth(t *testing.T) {
	t.Parallel()

	a := &action{tool: "compile", pkg: "github.com/foo/barbazqux"}
	for width := 1; width < 30; width++ {
		got := sliceLabel(timeSlice{from: 0, to: width, act: a})
		if lipgloss.Width(got) != width {
			t.Fatalf("width %d produced %d columns (%q)", width, lipgloss.Width(got), got)
		}
	}
}

func TestParseArgsModes(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		argv []string
		want Mode
	}{
		"the quiet screen is the default": {[]string{"build", "."}, ModeSimple},
		"verbose flag":                    {[]string{"-verbose", "build", "."}, ModeVerbose},
		"monitor flag":                    {[]string{"-monitor", "run", "."}, ModeMonitor},
		"simple flag":                     {[]string{"-simple", "build", "."}, ModeSimple},
		// -clear is about erasing a screen, so it implies there is one.
		"clear implies verbose":      {[]string{"-clear", "build", "."}, ModeVerbose},
		"clear leaves monitor alone": {[]string{"-clear", "-monitor", "run", "."}, ModeMonitor},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			opts, rest, err := parseArgs(tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			if opts.mode != tc.want {
				t.Fatalf("mode=%v, want %v", opts.mode, tc.want)
			}
			if len(rest) != 2 {
				t.Fatalf("rest=%v", rest)
			}
		})
	}

	t.Run("chart height is clamped to what the panel reserves", func(t *testing.T) {
		t.Parallel()

		opts, _, _ := parseArgs([]string{"-top", "99", "build", "."})
		if opts.top != maxSlowRows {
			t.Fatalf("top=%d, want %d", opts.top, maxSlowRows)
		}
	})

	// The report follows the screen: with -clear there is no screen to explain.
	t.Run("clear collapses the report to one line", func(t *testing.T) {
		t.Parallel()

		opts, _, _ := parseArgs([]string{"-clear", "build", "."})
		got := stripANSI(renderSummary(snap{compiled: 3}, nil, opts, 80))

		if strings.Contains(got, "SLOWEST") || strings.Contains(got, "CRITICAL PATH") {
			t.Fatalf("full report survived -clear: %q", got)
		}
		if !strings.Contains(got, "built 3 packages") {
			t.Fatalf("no summary at all: %q", got)
		}
	})
}

func TestIgnoreMatching(t *testing.T) {
	t.Parallel()

	list := parseIgnore([]string{"gitlab.com/tropicalsun/*", "example.com/internal"})

	hidden := []string{
		"gitlab.com/tropicalsun/foundation/packages/fnd-events/go",
		"gitlab.com/tropicalsun/anything",
		"example.com/internal",     // the package the pattern names
		"example.com/internal/api", // and the tree under it
	}
	for _, pkg := range hidden {
		if !list.match(pkg) {
			t.Fatalf("%q should be ignored", pkg)
		}
	}

	shown := []string{
		"gitlab.com/other/pkg",
		"example.com/internalise", // a prefix is not a path boundary
		"net/http",
		"",
	}
	for _, pkg := range shown {
		if list.match(pkg) {
			t.Fatalf("%q should not be ignored", pkg)
		}
	}

	// The wildcard spans slashes, which "path".Match would not do.
	if !globMatch("a/*/d", "a/b/c/d") {
		t.Fatal("* must cross path separators")
	}
	if globMatch("a/*/d", "a/b/c/e") {
		t.Fatal("suffix must still match")
	}
}

func TestIgnoreEnvAndFlagCombine(t *testing.T) {
	t.Setenv(ignoreEnv, "from.env/*, spaced.env/*")

	list := parseIgnore([]string{"from.flag/*"})

	for _, pkg := range []string{"from.env/x", "spaced.env/x", "from.flag/x"} {
		if !list.match(pkg) {
			t.Fatalf("%q should be ignored; flag and environment both apply", pkg)
		}
	}
}

// Ignored packages are still compiled, so they must stay in the totals — a
// progress bar that dropped them would never reach the end.
func TestIgnoredPackagesLeaveTheListsButNotTheTotals(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, parseIgnore([]string{"secret.com/*"}))

	st.start(startEvent{id: 1, tool: "compile", pkg: "secret.com/private/thing"})
	st.end(endEvent{id: 1, dur: 900 * time.Millisecond})
	st.start(startEvent{id: 2, tool: "compile", pkg: "example.com/app"})
	st.end(endEvent{id: 2, dur: 50 * time.Millisecond})

	sn := st.snapshot()

	if sn.compiled != 2 {
		t.Fatalf("compiled=%d, want both packages counted", sn.compiled)
	}
	if sn.cpu != 950*time.Millisecond {
		t.Fatalf("cpu=%v, want the ignored package included", sn.cpu)
	}

	for _, a := range sn.visibleSlow(true, 10) {
		if a.pkg != "example.com/app" {
			t.Fatalf("%q leaked into the slowest chart", a.pkg)
		}
	}
	for _, a := range sn.visibleRecent(true, 10) {
		if a.pkg != "example.com/app" {
			t.Fatalf("%q leaked into the done list", a.pkg)
		}
	}
	if len(sn.timeline) != 1 || sn.timeline[0].pkg != "example.com/app" {
		t.Fatalf("timeline carries an ignored package: %+v", sn.timeline)
	}
}

func TestIgnoredPackagesLeaveTheRunningListAndCriticalPath(t *testing.T) {
	t.Parallel()

	st := newStats(nil, true, parseIgnore([]string{"secret.com/*"}))
	st.start(startEvent{id: 1, tool: "compile", pkg: "secret.com/private/thing"})
	st.start(startEvent{id: 2, tool: "compile", pkg: "example.com/app"})

	sn := st.snapshot()
	if len(sn.active) != 1 || sn.active[0].pkg != "example.com/app" {
		t.Fatalf("running list shows an ignored package: %+v", sn.active)
	}

	graph := `[
	 {"ID":1,"Mode":"build","Package":"example.com/app","Deps":[2],"NeedBuild":true,
	  "TimeReady":"2026-01-01T00:00:01Z","TimeStart":"2026-01-01T00:00:01Z","TimeDone":"2026-01-01T00:00:02Z"},
	 {"ID":2,"Mode":"build","Package":"secret.com/private/thing","Deps":[],"NeedBuild":true,
	  "TimeReady":"2026-01-01T00:00:00Z","TimeStart":"2026-01-01T00:00:00Z","TimeDone":"2026-01-01T00:00:01Z"}
	]`

	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, []byte(graph), 0o600); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph(path, parseIgnore([]string{"secret.com/*"}))
	if err != nil {
		t.Fatal(err)
	}

	// The chain is two seconds long whether or not one of its links is named.
	if g.critical != 2*time.Second {
		t.Fatalf("critical=%v, want the ignored link still counted", g.critical)
	}
	for _, l := range g.path {
		if l.pkg != "example.com/app" {
			t.Fatalf("%q leaked into the critical path rows", l.pkg)
		}
	}
	for _, r := range g.roots {
		if r.pkg != "example.com/app" {
			t.Fatalf("%q leaked into the rebuilt roots", r.pkg)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}

			continue
		}
		b.WriteByte(s[i])
	}

	return b.String()
}

func TestFmtDur(t *testing.T) {
	t.Parallel()

	cases := map[time.Duration]string{
		0:                       "0s",
		250 * time.Millisecond:  "250ms",
		2500 * time.Millisecond: "2.50s",
		95 * time.Second:        "1m35s",
	}

	for in, want := range cases {
		if got := fmtDur(in); got != want {
			t.Fatalf("fmtDur(%v) = %q, want %q", in, got, want)
		}
	}
}

// The allocation rate is not reported by the runtime; it comes from the gap
// between what one collection left alive and what the next one found.
func TestAllocationRateFromGCTraces(t *testing.T) {
	t.Parallel()

	st := newMonitorState("app")

	st.mu.Lock()
	st.recordGCLocked(gcEvent{num: 1, at: time.Second, heapPrev: 4, heapLive: 2})
	st.recordGCLocked(gcEvent{num: 2, at: 3 * time.Second, heapPrev: 22, heapLive: 3})
	st.mu.Unlock()

	// 22 MB found minus 2 MB left alive, over two seconds.
	if got := st.snapshot().alloc.last; got != 10 {
		t.Fatalf("alloc rate = %v MB/s, want 10", got)
	}
}

// The live cpu figure has to come from a delta; ps only knows the lifetime
// average, which would be wrong for anything but a busy loop.
func TestCPUPercentFromDelta(t *testing.T) {
	t.Parallel()

	base := time.Now()
	prev := procSample{cpuTime: 2 * time.Second, at: base, ok: true}
	next := procSample{cpuTime: 3 * time.Second, at: base.Add(2 * time.Second), ok: true}

	pct, ok := next.cpuPercentSince(prev)
	if !ok || pct < 49 || pct > 51 {
		t.Fatalf("pct=%v ok=%v, want ~50", pct, ok)
	}

	if _, ok := next.cpuPercentSince(procSample{}); ok {
		t.Fatal("a missing previous sample must not produce a number")
	}
}

// The critical path is the chain that could not have been shortened by adding
// machines, so it must follow dependencies rather than raw durations.
func TestCriticalPathFollowsDependencies(t *testing.T) {
	t.Parallel()

	graph := `[
	 {"ID":1,"Mode":"build","Package":"root","Deps":[2,4],"NeedBuild":true,
	  "TimeReady":"2026-01-01T00:00:03Z","TimeStart":"2026-01-01T00:00:03Z","TimeDone":"2026-01-01T00:00:04Z"},
	 {"ID":2,"Mode":"build","Package":"slow-chain","Deps":[3],"NeedBuild":true,
	  "TimeReady":"2026-01-01T00:00:01Z","TimeStart":"2026-01-01T00:00:01Z","TimeDone":"2026-01-01T00:00:03Z"},
	 {"ID":3,"Mode":"build","Package":"leaf","Deps":[],"NeedBuild":false,
	  "TimeReady":"2026-01-01T00:00:00Z","TimeStart":"2026-01-01T00:00:00Z","TimeDone":"2026-01-01T00:00:01Z"},
	 {"ID":4,"Mode":"build","Package":"parallel","Deps":[],"NeedBuild":true,
	  "TimeReady":"2026-01-01T00:00:00Z","TimeStart":"2026-01-01T00:00:02Z","TimeDone":"2026-01-01T00:00:03Z"}
	]`

	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, []byte(graph), 0o600); err != nil {
		t.Fatal(err)
	}

	g, err := loadGraph(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	if g.built != 3 || g.cached != 1 {
		t.Fatalf("built=%d cached=%d, want 3/1", g.built, g.cached)
	}

	// leaf(1s) → slow-chain(2s) → root(1s) is 4s; "parallel" is off the path.
	if g.critical != 4*time.Second {
		t.Fatalf("critical=%v, want 4s", g.critical)
	}

	names := make([]string, 0, len(g.path))
	for _, l := range g.path {
		names = append(names, l.pkg)
	}

	if strings.Join(names, ",") != "root,slow-chain,leaf" {
		t.Fatalf("path=%v", names)
	}

	// "parallel" was ready at 0 but only started at 2s: it waited for a worker.
	if g.queueWait != 2*time.Second {
		t.Fatalf("queueWait=%v, want 2s", g.queueWait)
	}

	// A root is a package that rebuilt while all of its dependencies came from
	// the cache — its own sources changed. Both "slow-chain" (its only
	// dependency was cached) and "parallel" (it has none) qualify; "root" does
	// not, it only rebuilt because "slow-chain" did.
	roots := map[string]int{}
	for _, r := range g.roots {
		roots[r.pkg] = r.downstream
	}

	if len(roots) != 2 || roots["slow-chain"] != 1 || roots["parallel"] != 1 {
		t.Fatalf("roots=%+v", g.roots)
	}
}

func TestGoroutineLeakNoteOnlyForRelentlessGrowth(t *testing.T) {
	t.Parallel()

	s := newStyles()

	grow := monitorSnap{stacks: []stackGroup{{count: 40, where: "main.worker"}}}
	for _, n := range []float64{10, 20, 40, 80, 120, 200, 320, 500} {
		grow.goroutineTrend.push(n)
	}

	if note := goroutineLeakNote(s, grow); !strings.Contains(stripANSI(note), "main.worker") {
		t.Fatalf("growth was not reported: %q", stripANSI(note))
	}

	// A service that opens goroutines per request and closes them is not leaking.
	var busy monitorSnap
	for _, n := range []float64{10, 90, 20, 140, 30, 180, 25, 400} {
		busy.goroutineTrend.push(n)
	}

	if note := goroutineLeakNote(s, busy); note != "" {
		t.Fatalf("load mistaken for a leak: %q", stripANSI(note))
	}
}

// ps reports cumulative cpu time in whichever shape fits the number.
func TestParseCPUTime(t *testing.T) {
	t.Parallel()

	cases := map[string]time.Duration{
		"12.34":      12*time.Second + 340*time.Millisecond,
		"01:23.45":   83*time.Second + 450*time.Millisecond,
		"1:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00": 49 * time.Hour,
	}

	for in, want := range cases {
		got, ok := parseCPUTime(in)
		if !ok || got != want {
			t.Fatalf("parseCPUTime(%q) = %v (ok=%v), want %v", in, got, ok, want)
		}
	}

	if _, ok := parseCPUTime("nonsense"); ok {
		t.Fatal("expected failure")
	}
}

// The live cpu figure has to come from a delta; ps only knows the lifetime
// average, which would be wrong for anything but a busy loop.

func TestParseCompileBench(t *testing.T) {
	t.Parallel()

	// Both halves report a subtotal; adding the individual phases as well would
	// double the time, which is the bug this locks down.
	const sample = `commit: go1.26.3
BenchmarkCompile:gopkg.in/yaml.v3:fe:parse         1  222436583 ns/op  27.85 %  11298 lines  50792 lines/s
BenchmarkCompile:gopkg.in/yaml.v3:fe:escapes       1   20520625 ns/op   2.57 %
BenchmarkCompile:gopkg.in/yaml.v3:fe:subtotal      1  271388250 ns/op  33.98 %
BenchmarkCompile:gopkg.in/yaml.v3:be:compilefuncs  1  501222625 ns/op  62.76 %    395 funcs    788 funcs/s
BenchmarkCompile:gopkg.in/yaml.v3:be:dumpobj       1   26067958 ns/op   3.26 %
BenchmarkCompile:gopkg.in/yaml.v3:be:subtotal      1  527290583 ns/op  66.02 %
BenchmarkCompile:gopkg.in/yaml.v3:total            1  798678833 ns/op 100.00 %
`

	path := filepath.Join(t.TempDir(), "bench.txt")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path) //nolint:gosec // path is from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	got := parseCompileBench(f)

	if got.frontend != 271388250 || got.backend != 527290583 {
		t.Fatalf("frontend=%v backend=%v", got.frontend, got.backend)
	}
	if got.lines != 11298 || got.funcs != 395 {
		t.Fatalf("lines=%d funcs=%d", got.lines, got.funcs)
	}
}

func TestParseGCTrace(t *testing.T) {
	t.Parallel()

	line := "gc 7 @0.512s 3%: 0.018+0.34+0.003 ms clock, 0.14+0.10/0.31/0.53+0.028 ms cpu, " +
		"12->13->6 MB, 14 MB goal, 0 MB stacks, 0 MB globals, 8 P"

	ev, ok := parseGCTrace(line)
	if !ok {
		t.Fatal("not recognised as a gc trace")
	}

	if ev.num != 7 || ev.at != 512*time.Millisecond || ev.gcCPU != 3 {
		t.Fatalf("num=%d at=%v cpu=%v", ev.num, ev.at, ev.gcCPU)
	}
	if ev.heapPrev != 12 || ev.heapLive != 6 || ev.goal != 14 {
		t.Fatalf("heap %v->%v goal %v", ev.heapPrev, ev.heapLive, ev.goal)
	}
	// Only the two stop-the-world phases: 0.018 + 0.003 ms.
	if ev.pause < 20*time.Microsecond || ev.pause > 22*time.Microsecond {
		t.Fatalf("pause=%v, want the stop-the-world phases only", ev.pause)
	}

	if _, ok := parseGCTrace("request 12 handled"); ok {
		t.Fatal("ordinary output must not parse as a gc trace")
	}
}

func TestParseGoroutineProfile(t *testing.T) {
	t.Parallel()

	body := `goroutine profile: total 9
6 @ 0x104 0x109 0x120
#	0x103	runtime.gopark+0x11	/go/src/runtime/proc.go:1
#	0x108	main.leakedWorker+0x1c	/app/main.go:12
#	0x119	main.main+0x40	/app/main.go:30

2 @ 0x204
#	0x203	net/http.(*conn).serve+0x8	/go/src/net/http/server.go:1

1 @ 0x304
#	0x303	runtime.goexit+0x1	/go/src/runtime/asm.s:1
`

	total, groups := parseGoroutineProfile(body)
	if total != 9 {
		t.Fatalf("total=%d, want 9", total)
	}

	if len(groups) != 3 {
		t.Fatalf("got %d groups", len(groups))
	}

	// Largest first, and labelled with the program's own frame rather than the
	// runtime.gopark every blocked goroutine sits in.
	if groups[0].count != 6 || groups[0].where != "main.leakedWorker" {
		t.Fatalf("%+v", groups[0])
	}
	if groups[1].where != "net/http.(*conn).serve" {
		t.Fatalf("%+v", groups[1])
	}
	if groups[2].where != "(runtime)" {
		t.Fatalf("%+v", groups[2])
	}
}

// The allocation rate is not reported by the runtime; it comes from the gap
// between what one collection left alive and what the next one found.

func TestParseInitTrace(t *testing.T) {
	t.Parallel()

	ev, ok := parseInitTrace("init internal/godebug @0.43 ms, 0.22 ms clock, 2176 bytes, 44 allocs")
	if !ok {
		t.Fatal("not recognised as an init trace")
	}

	if ev.pkg != "internal/godebug" || ev.clock != 220*time.Microsecond ||
		ev.at != 430*time.Microsecond || ev.bytes != 2176 || ev.allocs != 44 {
		t.Fatalf("%+v", ev)
	}

	for _, line := range []string{"initialising things", "init", "hello"} {
		if _, ok := parseInitTrace(line); ok {
			t.Fatalf("%q must not parse as an init trace", line)
		}
	}
}

// The wait reason is what tells a leak apart from work, and it may contain
// brackets of its own.

// The wait reason is what tells a leak apart from work, and it may contain
// brackets of its own.
func TestParseSchedGoroutine(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"  G12: status=1(chan receive) m=nil lockedm=nil":    "chan receive",
		"  G3: status=4(GC worker (idle)) m=nil lockedm=nil": "GC worker (idle)",
		"  G1: status=2() m=0 lockedm=0":                     "",
		"G7: status=4(select) m=nil lockedm=nil":             "select",
	}

	for line, want := range cases {
		got, ok := parseSchedGoroutine(line)
		if !ok || got != want {
			t.Fatalf("%q → %q (ok=%v), want %q", line, got, ok, want)
		}
	}

	for _, line := range []string{
		"  P0: status=1 schedtick=6 runqsize=0",
		"  M4: p=0 curg=18 mallocing=0",
		"SCHED 1004ms: gomaxprocs=8",
		"Good morning",
	} {
		if _, ok := parseSchedGoroutine(line); ok {
			t.Fatalf("%q must not be read as a goroutine line", line)
		}
	}
}

func TestParseSchedTrace(t *testing.T) {
	t.Parallel()

	line := "SCHED 1004ms: gomaxprocs=8 idleprocs=7 threads=6 spinningthreads=0 idlethreads=3 runqueue=2 [0 1 0]"

	s, ok := parseSchedTrace(line)
	if !ok || s.procs != 8 || s.threads != 6 || s.idle != 7 || s.runqueue != 2 {
		t.Fatalf("ok=%v %+v", ok, s)
	}

	if _, ok := parseSchedTrace("hello world"); ok {
		t.Fatal("ordinary output must not parse as a sched trace")
	}
}

// ps reports cumulative cpu time in whichever shape fits the number.

func TestParseTestLine(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		status testStatus
		dur    time.Duration
	}{
		"ok  \tfnd-app/internal/api\t0.123s":   {testPassed, 123 * time.Millisecond},
		"ok  \tfnd-app/internal/api\t(cached)": {testCached, 0},
		"FAIL\tfnd-app/internal/api\t0.456s":   {testFailed, 456 * time.Millisecond},
		"?   \tfnd-app/cmd\t[no test files]":   {testNoTests, 0},
	}

	for line, want := range cases {
		got, ok := parseTestLine(line)
		if !ok || got.status != want.status || got.dur != want.dur {
			t.Fatalf("%q → %+v ok=%v, want status %v dur %v", line, got, ok, want.status, want.dur)
		}
	}

	for _, line := range []string{"=== RUN   TestFoo", "--- FAIL: TestFoo (0.00s)", "FAIL", ""} {
		if _, ok := parseTestLine(line); ok {
			t.Fatalf("%q must not be read as a package verdict", line)
		}
	}

	if !isTestFailure("--- FAIL: TestFoo (0.00s)") || isTestFailure("--- PASS: TestFoo (0.00s)") {
		t.Fatal("failing test functions are not recognised")
	}
}

// The critical path is the chain that could not have been shortened by adding
// machines, so it must follow dependencies rather than raw durations.

func TestSplitRunArgs(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in    []string
		flags []string
		pkg   []string
		args  []string
	}{
		"package then program arguments": {
			[]string{".", "serve", "--port=8080"},
			nil, []string{"."}, []string{"serve", "--port=8080"},
		},
		"build flag with a separate value": {
			[]string{"-tags", "dynamic", "./cmd/app", "serve"},
			[]string{"-tags", "dynamic"}, []string{"./cmd/app"}, []string{"serve"},
		},
		"build flag with an inline value": {
			[]string{"-ldflags=-s -w", ".", "serve"},
			[]string{"-ldflags=-s -w"}, []string{"."}, []string{"serve"},
		},
		"a list of go files": {
			[]string{"a.go", "b.go", "serve"},
			nil, []string{"a.go", "b.go"}, []string{"serve"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			flags, pkg, args := splitRunArgs(tc.in)
			if strings.Join(flags, " ") != strings.Join(tc.flags, " ") ||
				strings.Join(pkg, " ") != strings.Join(tc.pkg, " ") ||
				strings.Join(args, " ") != strings.Join(tc.args, " ") {
				t.Fatalf("flags=%v pkg=%v args=%v", flags, pkg, args)
			}
		})
	}
}
