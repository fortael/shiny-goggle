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

	cases := map[string]struct {
		in   []string
		want []string
	}{
		"bare subcommand gets go":  {[]string{"build", "./..."}, []string{"go", "build", "./..."}},
		"explicit go is kept":      {[]string{"go", "run", "."}, []string{"go", "run", "."}},
		"absolute go is kept":      {[]string{"/usr/local/go/bin/go", "test"}, []string{"/usr/local/go/bin/go", "test"}},
		"unknown command untouche": {[]string{"make", "server"}, []string{"make", "server"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := normalizeGoCommand(tc.in)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
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
