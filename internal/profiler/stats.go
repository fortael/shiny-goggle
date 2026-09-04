package profiler

import (
	"sort"
	"sync"
	"time"
)

const (
	keptSlow     = 64   // slowest actions retained, the chart shows a slice of this
	keptBlocking = 64   // likewise for the actions that held the build alone
	keptRecent   = 32   // finished actions retained for the "done" list
	keptDiag     = 400  // captured go-command diagnostic lines
	keptSamples  = 2048 // parallelism samples for the sparkline
	keptTimeline = 8192 // actions retained for the timeline; a huge build fits

	// soloThreshold is how long a package has to be the only thing compiling
	// before it is worth naming: below that it is scheduling noise.
	soloThreshold = 100 * time.Millisecond
)

// action is a single tool invocation (compile/asm/cgo/link) of one package.
type action struct {
	id     uint64
	tool   string
	pkg    string
	std    bool
	start  time.Time
	dur    time.Duration
	failed bool
	hidden bool          // matched -ignore: still built and counted, never listed
	expect time.Duration // typical duration from previous builds, 0 if unknown
	// solo is the wall clock time this action spent as the only thing in
	// flight: the build made no other progress while it ran.
	solo time.Duration
	// Compiler internals, from `compile -bench`; zero when unavailable.
	frontend time.Duration // parsing, type checking, inlining, escape analysis
	backend  time.Duration // generating and writing machine code
	lines    int           // source lines the compiler read
	funcs    int           // functions it generated — generics multiply this
}

// density is how many functions the compiler generated per source line. The
// absolute figure means nothing on its own — a typical package sits around 0.1 —
// so it is only ever read against the build's own average, see snap.density.
func (a action) density() float64 {
	if a.lines <= 0 || a.funcs <= 0 {
		return 0
	}

	return float64(a.funcs) / float64(a.lines)
}

// blocking reports whether this action held the whole build to itself long
// enough to be worth naming.
func (a action) blocking() bool { return a.solo >= soloThreshold }

// running returns how long an in-flight action has been going.
func (a action) running(now time.Time) time.Duration { return now.Sub(a.start) }

// stats aggregates every event of one build. It is written from the socket
// server and the stderr reader, and read by whichever renderer is active, so
// every field lives behind the mutex and readers work on snapshots.
type stats struct {
	mu sync.RWMutex

	startedAt  time.Time
	finishedAt time.Time

	active   map[uint64]*action
	slow     []*action
	blocking []*action
	recent   []*action
	timeline []*action
	diag     []string

	compiled   int
	std        int
	failed     int
	aborted    int
	peakPar    int
	cpu        time.Duration
	samples    []uint16
	tools      map[string]int
	toolTime   map[string]time.Duration
	tests      []testResult
	testFails  int
	heavy      []*action // packages with compiler phase data, slowest first
	totalFuncs int
	totalLines int

	// The build's concurrency is piecewise constant between the moments an
	// action starts or ends, so serial and idle time can be accounted exactly
	// instead of sampled. See closeInterval.
	lastChange time.Time
	serial     time.Duration // exactly one action in flight
	idle       time.Duration // nothing in flight at all

	linkDone  bool
	exitCode  int
	exitKnown bool
	keepAll   bool // record the timeline; off in the simple mode
	hasPlan   bool
	// compileSeen is the union of the packages the go command said it would
	// compile and the ones we have watched it compile — the denominator of the
	// progress bar. See planPackages for why the union is the exact answer.
	compileSeen map[string]bool
	ignore      ignoreList
	expect      map[string]time.Duration
	slower      []*action       // finished much slower than the recorded expectation
	stdPkgs     map[string]bool // import paths the go command marked as standard library
}

func newStats(expect map[string]time.Duration, keepAll bool, ignore ignoreList) *stats {
	now := time.Now()

	return &stats{
		startedAt:   now,
		lastChange:  now,
		active:      make(map[uint64]*action, 32),
		tools:       make(map[string]int, 8),
		toolTime:    make(map[string]time.Duration, 8),
		stdPkgs:     make(map[string]bool, 128),
		compileSeen: make(map[string]bool, 256),
		keepAll:     keepAll,
		ignore:      ignore,
		expect:      expect,
	}
}

// addPlan merges the go command's own build plan into the denominator.
func (s *stats) addPlan(pkgs map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for pkg := range pkgs {
		s.compileSeen[pkg] = true
	}

	s.hasPlan = true
}

// closeInterval books the time since the active set last changed. Between two
// such moments the number of tools in flight is constant, so this is exact
// accounting, not sampling: it must be called before every mutation of
// s.active, and may be called at any other moment to refresh the live numbers.
func (s *stats) closeInterval(now time.Time) {
	dt := now.Sub(s.lastChange)
	s.lastChange = now

	if dt <= 0 {
		return
	}

	switch len(s.active) {
	case 0:
		s.idle += dt
	case 1:
		s.serial += dt
		for _, a := range s.active {
			a.solo += dt
		}
	}
}

func (s *stats) start(ev startEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.closeInterval(now)

	// Only the compiler is told -std. asm/cgo run right after the compile step
	// of the same package, so by now we already know where it belongs.
	std := ev.std
	if std {
		s.stdPkgs[ev.pkg] = true
	} else if ev.tool != "compile" {
		std = s.stdPkgs[ev.pkg]
	}

	if ev.tool == "compile" && ev.pkg != "" {
		s.compileSeen[ev.pkg] = true
	}

	a := &action{
		id:     ev.id,
		tool:   ev.tool,
		pkg:    ev.pkg,
		std:    std,
		start:  now,
		hidden: s.ignore.match(ev.pkg),
	}
	if d, ok := s.expect[ev.pkg]; ok {
		a.expect = d
	}

	s.active[ev.id] = a
	if len(s.active) > s.peakPar {
		s.peakPar = len(s.active)
	}
}

// end closes an in-flight action and reports which tool it belonged to, so the
// driver can react to the linker finishing.
func (s *stats) end(ev endEvent) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closeInterval(time.Now())

	a, ok := s.active[ev.id]
	if !ok {
		return "", false
	}
	delete(s.active, ev.id)

	if ev.aborted {
		s.aborted++

		return a.tool, false
	}

	a.dur = ev.dur
	a.failed = ev.fail
	a.frontend, a.backend = ev.frontend, ev.backend
	a.lines, a.funcs = ev.lines, ev.funcs

	// The shim times the tool itself, we time the gaps around it; never let
	// bookkeeping make an action look like it blocked longer than it ran.
	a.solo = min(a.solo, a.dur)

	s.cpu += ev.dur
	s.tools[a.tool]++
	s.toolTime[a.tool] += ev.dur

	// "Packages" means compile actions; asm/cgo/link are separate work on a
	// package that was already counted.
	switch {
	case a.tool == "link":
		s.linkDone = true
	case a.tool != "compile":
	case a.std:
		s.std++
	default:
		s.compiled++
	}

	if ev.fail {
		s.failed++
	}

	if a.expect > 250*time.Millisecond && a.dur > a.expect*3/2 && !a.hidden {
		s.slower = append(s.slower, a)
	}

	s.pushRecent(a)
	s.pushSlow(a)

	if a.blocking() && !a.hidden {
		s.blocking = insertSorted(s.blocking, a, keptBlocking, func(x *action) time.Duration { return x.solo })
	}

	if a.funcs > 0 && !a.hidden {
		// The build's own average is the only sensible yardstick for "unusually
		// many functions for its size" — the absolute ratio means nothing.
		s.totalFuncs += a.funcs
		s.totalLines += a.lines

		s.heavy = insertSorted(s.heavy, a, keptSlow, func(x *action) time.Duration {
			return x.frontend + x.backend
		})
	}

	if s.keepAll && !a.hidden && len(s.timeline) < keptTimeline {
		s.timeline = append(s.timeline, a)
	}

	return a.tool, true
}

func (s *stats) pushRecent(a *action) {
	if a.hidden {
		return
	}

	s.recent = append(s.recent, a)
	if len(s.recent) > keptRecent {
		s.recent = s.recent[len(s.recent)-keptRecent:]
	}
}

// pushSlow keeps s.slow sorted by duration, longest first.
func (s *stats) pushSlow(a *action) {
	if a.hidden {
		return
	}

	s.slow = insertSorted(s.slow, a, keptSlow, func(x *action) time.Duration { return x.dur })
}

// insertSorted keeps a capped leaderboard ordered by key, largest first.
func insertSorted(list []*action, a *action, capacity int, key func(*action) time.Duration) []*action {
	if n := len(list); n >= capacity && key(a) <= key(list[n-1]) {
		return list
	}

	i := sort.Search(len(list), func(i int) bool { return key(list[i]) < key(a) })
	list = append(list, nil)
	copy(list[i+1:], list[i:])
	list[i] = a

	if len(list) > capacity {
		list = list[:capacity]
	}

	return list
}

// observeTestLine picks the package verdicts out of `go test` output. The line
// itself is still forwarded verbatim once the screen is handed back.
func (s *stats) observeTestLine(line string) {
	if isTestFailure(line) {
		s.mu.Lock()
		s.testFails++
		s.mu.Unlock()

		return
	}

	res, ok := parseTestLine(line)
	if !ok {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tests = append(s.tests, res)
}

func (s *stats) addDiag(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.diag = append(s.diag, line)
	if len(s.diag) > keptDiag {
		s.diag = s.diag[len(s.diag)-keptDiag:]
	}
}

// sample records the current parallelism; called on the UI tick.
func (s *stats) sample() {
	s.mu.Lock()
	defer s.mu.Unlock()

	//nolint:gosec // parallelism never approaches uint16 overflow
	s.samples = append(s.samples, uint16(len(s.active)))
	if len(s.samples) > keptSamples {
		s.samples = s.samples[len(s.samples)-keptSamples:]
	}
}

func (s *stats) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.finishedAt.IsZero() {
		s.finishedAt = time.Now()
	}
}

// setExit records the go command's exit status. It is only known in time for
// the summary when the go command outlives the build phase — with `go run` the
// program is still running when we hand the terminal back.
func (s *stats) setExit(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.exitCode, s.exitKnown = code, true
}

// snap is an immutable view of stats used by the renderers.
type snap struct {
	now        time.Time
	startedAt  time.Time
	elapsed    time.Duration
	active     []action
	slow       []*action
	blocking   []*action
	recent     []*action
	timeline   []*action
	diag       []string
	slower     []*action
	serial     time.Duration
	idle       time.Duration
	compiled   int
	std        int
	failed     int
	aborted    int
	peakPar    int
	cpu        time.Duration
	samples    []uint16
	tools      map[string]int
	toolTime   map[string]time.Duration
	tests      []testResult
	testFails  int
	heavy      []*action
	totalFuncs int
	totalLines int
	linkDone   bool
	done       bool
	failedGo   bool // the go command itself came back non-zero
	planned    int  // packages this build will compile in total, 0 while unknown
}

// progress is the share of the build that is done, and whether it is known at
// all: without the go command's own plan there is no honest denominator.
func (s snap) progress() (float64, bool) {
	if s.planned <= 0 {
		return 0, false
	}

	return min(float64(s.totalPackages())/float64(s.planned), 1), true
}

func (s *stats) snapshot() snap {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Book the time since the last change so a package that is blocking the
	// build right now is already visible as such.
	s.closeInterval(time.Now())

	// The go command runs `asm -gensymabis` before it compiles a package, so
	// the first action of a stdlib package arrives before we learn that the
	// package is stdlib at all. Settle those flags now that we know more.
	for _, a := range s.slow {
		a.std = a.std || s.stdPkgs[a.pkg]
	}
	for _, a := range s.recent {
		a.std = a.std || s.stdPkgs[a.pkg]
	}

	now := time.Now()
	end := now
	if !s.finishedAt.IsZero() {
		end = s.finishedAt
	}

	sn := snap{
		now:        now,
		startedAt:  s.startedAt,
		elapsed:    end.Sub(s.startedAt),
		active:     make([]action, 0, len(s.active)),
		slow:       append([]*action(nil), s.slow...),
		blocking:   append([]*action(nil), s.blocking...),
		recent:     append([]*action(nil), s.recent...),
		serial:     s.serial,
		idle:       s.idle,
		timeline:   append([]*action(nil), s.timeline...),
		diag:       append([]string(nil), s.diag...),
		slower:     append([]*action(nil), s.slower...),
		compiled:   s.compiled,
		std:        s.std,
		failed:     s.failed,
		aborted:    s.aborted,
		peakPar:    s.peakPar,
		cpu:        s.cpu,
		samples:    append([]uint16(nil), s.samples...),
		tests:      append([]testResult(nil), s.tests...),
		testFails:  s.testFails,
		totalFuncs: s.totalFuncs,
		totalLines: s.totalLines,
		heavy:      append([]*action(nil), s.heavy...),
		tools:      make(map[string]int, len(s.tools)),
		toolTime:   make(map[string]time.Duration, len(s.toolTime)),
		linkDone:   s.linkDone,
		done:       !s.finishedAt.IsZero(),
		failedGo:   s.exitKnown && s.exitCode != 0,
	}

	if s.hasPlan {
		sn.planned = len(s.compileSeen)
	}

	for _, a := range s.active {
		if a.hidden {
			continue
		}

		copied := *a
		copied.std = copied.std || s.stdPkgs[copied.pkg]
		sn.active = append(sn.active, copied)
	}
	// Longest running first: that is the package everyone is waiting for.
	sort.Slice(sn.active, func(i, j int) bool { return sn.active[i].start.Before(sn.active[j].start) })

	// An action that is blocking the build right now belongs in the table too,
	// otherwise the very package you are staring at is the one missing from it.
	for i := range sn.active {
		if sn.active[i].blocking() {
			sn.blocking = append(sn.blocking, &sn.active[i])
		}
	}
	sort.SliceStable(sn.blocking, func(i, j int) bool { return sn.blocking[i].solo > sn.blocking[j].solo })

	for k, v := range s.tools {
		sn.tools[k] = v
		sn.toolTime[k] = s.toolTime[k]
	}

	return sn
}

// totalPackages counts every compile action, std or not.
func (s snap) totalPackages() int { return s.compiled + s.std }

// parallelism is the average number of tools that ran concurrently.
func (s snap) parallelism() float64 {
	if s.elapsed <= 0 {
		return 0
	}

	return s.cpu.Seconds() / s.elapsed.Seconds()
}

// density is the whole build's functions-per-line, the yardstick a single
// package is compared against.
func (s snap) density() float64 {
	if s.totalLines <= 0 || s.totalFuncs <= 0 {
		return 0
	}

	return float64(s.totalFuncs) / float64(s.totalLines)
}

// serialShare is how much of the build was one package compiling on its own,
// with every other core idle. It is the part of the wall clock that more CPUs
// would not have helped with.
func (s snap) serialShare() float64 {
	if s.elapsed <= 0 {
		return 0
	}

	return s.serial.Seconds() / s.elapsed.Seconds()
}

// visibleBlocking lists the actions that held the build alone, longest first.
func (s snap) visibleBlocking(n int) []*action {
	if len(s.blocking) <= n {
		return s.blocking
	}

	return s.blocking[:n]
}

// concentration returns the share of compile time spent in the top n packages.
func (s snap) concentration(n int) float64 {
	if s.cpu <= 0 || len(s.slow) == 0 {
		return 0
	}

	var top time.Duration
	for i, a := range s.slow {
		if i >= n {
			break
		}
		top += a.dur
	}

	return top.Seconds() / s.cpu.Seconds()
}

// visibleSlow filters the chart entries by the "show stdlib" toggle.
func (s snap) visibleSlow(withStd bool, n int) []*action {
	out := make([]*action, 0, n)
	for _, a := range s.slow {
		if !withStd && a.std {
			continue
		}
		out = append(out, a)
		if len(out) == n {
			break
		}
	}

	return out
}

// visibleRecent lists the packages that just finished, newest first. Only
// compiling and linking are shown: the assembler steps in between would push
// everything interesting out of such a short list.
func (s snap) visibleRecent(withStd bool, n int) []*action {
	out := make([]*action, 0, n)
	for i := len(s.recent) - 1; i >= 0 && len(out) < n; i-- {
		a := s.recent[i]
		if a.tool != "compile" && a.tool != "link" {
			continue
		}
		if !withStd && a.std {
			continue
		}
		out = append(out, a)
	}

	return out
}

// activeRows returns at most n in-flight actions, longest running first.
func (s snap) activeRows(n int) []action {
	if len(s.active) <= n {
		return s.active
	}

	return s.active[:n]
}
