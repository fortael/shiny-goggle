package profiler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
)

const (
	monitorTick  = 500 * time.Millisecond
	keptOutput   = 5000 // program output lines held for scrolling
	keptSeries   = 240  // samples per chart, two minutes at the tick above
	keptGCEvents = 200
)

// buildValueFlags are the `go run` flags that take a separate argument, which
// has to be skipped when working out where the package specification starts.
var buildValueFlags = map[string]bool{
	"-o": true, "-p": true, "-tags": true, "-ldflags": true, "-gcflags": true,
	"-asmflags": true, "-gccgoflags": true, "-covermode": true, "-coverpkg": true,
	"-exec": true, "-mod": true, "-modfile": true, "-overlay": true, "-pkgdir": true,
	"-toolexec": true, "-buildmode": true, "-compiler": true, "-installsuffix": true,
	"-pgo": true, "-C": true,
}

// splitRunArgs separates `go run [flags] <package> [program arguments]`. The
// package is the first argument that is not a flag, or the run of .go files
// that starts there.
func splitRunArgs(args []string) (flags, pkg, programArgs []string) {
	i := 0

	for i < len(args) && strings.HasPrefix(args[i], "-") {
		flag := args[i]
		flags = append(flags, flag)
		i++

		if name, _, hasValue := strings.Cut(flag, "="); !hasValue && buildValueFlags[name] && i < len(args) {
			flags = append(flags, args[i])
			i++
		}
	}

	if i >= len(args) {
		return flags, nil, nil
	}

	if strings.HasSuffix(args[i], ".go") {
		for i < len(args) && strings.HasSuffix(args[i], ".go") {
			pkg = append(pkg, args[i])
			i++
		}
	} else {
		pkg = append(pkg, args[i])
		i++
	}

	return flags, pkg, args[i:]
}

// series is a fixed length window of samples for a sparkline.
type series struct {
	values []float64
	last   float64
}

func (s *series) push(v float64) {
	s.last = v
	s.values = append(s.values, v)

	if len(s.values) > keptSeries {
		s.values = s.values[len(s.values)-keptSeries:]
	}
}

func (s *series) peak() float64 {
	var top float64
	for _, v := range s.values {
		top = max(top, v)
	}

	return top
}

// monitorState is everything the monitor knows about the running program. It is
// written by the samplers and the output readers, and read by the renderer.
type monitorState struct {
	mu sync.RWMutex

	started time.Time
	pid     int
	cmdline string

	rss   series
	cpu   series
	heap  series
	alloc series // MB per second, derived from consecutive gc traces
	gcs   []gcEvent
	sched schedSample
	inits []initEvent

	goroutines     int
	goroutineTrend series
	waitReasons    map[string]int
	stacks         []stackGroup

	// A scheddetail block arrives as one goroutine line after another; they are
	// accumulated here and committed when the next SCHED header shows up.
	pendingCount   int
	pendingReasons map[string]int

	pprofAddr   string
	pprofNote   string
	cpuProfile  string
	heapProfile string

	output  []string
	exited  bool
	exitErr string
}

func newMonitorState(cmdline string) *monitorState {
	return &monitorState{started: time.Now(), cmdline: cmdline}
}

func (m *monitorState) addOutput(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.output = append(m.output, line)
	if len(m.output) > keptOutput {
		m.output = m.output[len(m.output)-keptOutput:]
	}
}

// observeRuntimeLine peels the GODEBUG traces out of the program's stderr. They
// are the price of admission for zero-configuration monitoring, and the user
// never asked to read them, so they do not reach the output tab.
func (m *monitorState) observeRuntimeLine(line string) bool {
	if ev, ok := parseGCTrace(line); ok {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.recordGCLocked(ev)

		return true
	}

	if s, ok := parseSchedTrace(line); ok {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.sched = s
		m.commitGoroutinesLocked()

		return true
	}

	if reason, ok := parseSchedGoroutine(line); ok {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.pendingCount++
		if reason != "" {
			if m.pendingReasons == nil {
				m.pendingReasons = map[string]int{}
			}
			m.pendingReasons[reason]++
		}

		return true
	}

	if ev, ok := parseInitTrace(line); ok {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.inits = append(m.inits, ev)

		return true
	}

	// Everything else scheddetail prints (P and M lines) is noise here, but it
	// must not reach the output tab either.
	trimmed := strings.TrimSpace(line)

	return strings.HasPrefix(trimmed, "P") && strings.Contains(trimmed, "runqsize=") ||
		strings.HasPrefix(trimmed, "M") && strings.Contains(trimmed, "mallocing=")
}

// recordGCLocked stores a collection and derives the allocation rate from the
// gap to the previous one: the heap climbed from what was left alive last time
// to what was there when this cycle started, all of it freshly allocated.
func (m *monitorState) recordGCLocked(ev gcEvent) {
	if n := len(m.gcs); n > 0 {
		prev := m.gcs[n-1]
		if window := ev.at - prev.at; window > 0 {
			m.alloc.push(max(ev.heapPrev-prev.heapLive, 0) / window.Seconds())
		}
	}

	m.heap.push(ev.heapLive)
	m.gcs = append(m.gcs, ev)

	if len(m.gcs) > keptGCEvents {
		m.gcs = m.gcs[len(m.gcs)-keptGCEvents:]
	}
}

// commitGoroutinesLocked closes off a scheddetail block.
func (m *monitorState) commitGoroutinesLocked() {
	if m.pendingCount == 0 {
		return
	}

	m.setGoroutinesLocked(m.pendingCount)
	m.waitReasons = m.pendingReasons
	m.pendingCount, m.pendingReasons = 0, nil
}

func (m *monitorState) setGoroutinesLocked(n int) {
	m.goroutines = n
	m.goroutineTrend.push(float64(n))
}

func (m *monitorState) addSample(s, prev procSample) {
	if !s.ok {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.rss.push(s.rssKB / 1024)

	if pct, ok := s.cpuPercentSince(prev); ok {
		m.cpu.push(pct)
	}
}

func (m *monitorState) setExited(err string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.exited, m.exitErr = true, err
}

// monitorSnap is the renderer's read-only view.
type monitorSnap struct {
	uptime         time.Duration
	pid            int
	cmdline        string
	rss            series
	cpu            series
	heap           series
	alloc          series
	gcs            []gcEvent
	sched          schedSample
	inits          []initEvent
	goroutines     int
	goroutineTrend series
	waitReasons    map[string]int
	stacks         []stackGroup
	pprofAddr      string
	pprofNote      string
	cpuProfile     string
	heapProfile    string
	output         []string
	exited         bool
	exitErr        string
}

func (m *monitorState) snapshot() monitorSnap {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return monitorSnap{
		uptime:         time.Since(m.started),
		pid:            m.pid,
		cmdline:        m.cmdline,
		rss:            m.rss,
		cpu:            m.cpu,
		heap:           m.heap,
		alloc:          m.alloc,
		gcs:            append([]gcEvent(nil), m.gcs...),
		sched:          m.sched,
		inits:          append([]initEvent(nil), m.inits...),
		goroutines:     m.goroutines,
		goroutineTrend: m.goroutineTrend,
		waitReasons:    maps.Clone(m.waitReasons),
		stacks:         append([]stackGroup(nil), m.stacks...),
		pprofAddr:      m.pprofAddr,
		pprofNote:      m.pprofNote,
		cpuProfile:     m.cpuProfile,
		heapProfile:    m.heapProfile,
		output:         append([]string(nil), m.output...),
		exited:         m.exited,
		exitErr:        m.exitErr,
	}
}

// runMonitor builds the program with the usual profiler screen and then keeps
// the terminal, showing what the program does while it runs.
//
// `go run` is rewritten into `go build -o <tmp>` on purpose: owning the process
// ourselves is what makes the pid, the signals and the resource numbers exact —
// with `go run` the program is a grandchild we would have to go looking for.
func runMonitor(goArgs []string, self string, opts *options) int {
	if len(goArgs) < 2 || goArgs[1] != "run" {
		fmt.Fprintln(os.Stderr, "shiny-goggles: -monitor watches a program, so it needs `go run <package>`")

		full, _, ok := injectToolexec(goArgs, self, opts)
		if !ok {
			return execPassthrough(goArgs)
		}

		return runInstrumented(full, goArgs, goArgs[1], opts)
	}

	flags, pkg, programArgs := splitRunArgs(goArgs[2:])
	if len(pkg) == 0 {
		fmt.Fprintln(os.Stderr, "shiny-goggles: no package to build")

		return 2
	}

	dir, err := os.MkdirTemp("", "shiny-goggles-monitor-*")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	bin := filepath.Join(dir, "program")

	buildArgs := append([]string{"go", "build"}, flags...)
	buildArgs = append(buildArgs, "-o", bin)
	buildArgs = append(buildArgs, pkg...)

	full, _, ok := injectToolexec(buildArgs, self, opts)
	if !ok {
		return execPassthrough(buildArgs)
	}

	if code := runInstrumented(full, buildArgs, "build", opts); code != 0 {
		return code
	}

	return watchProgram(bin, programArgs, strings.Join(goArgs, " "), opts)
}

// watchProgram runs the freshly built binary under the monitor screen.
func watchProgram(bin string, args []string, cmdline string, opts *options) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := newMonitorState(cmdline)

	var (
		proc       atomic.Pointer[os.Process]
		interrupts atomic.Int32
	)

	stop := func() {
		p := proc.Load()
		if p == nil {
			return
		}

		sig := os.Signal(os.Interrupt)
		if interrupts.Add(1) > 1 {
			sig = os.Kill
		}
		_ = p.Signal(sig)
	}

	done := make(chan int, 1)

	go func() {
		code, err := runProgram(ctx, bin, args, st, opts, func(p *os.Process) { proc.Store(p) })
		if err != nil {
			fmt.Fprintf(os.Stderr, "shiny-goggles: %v\n", err)
		}
		done <- code
	}()

	if !opts.plain && term.IsTerminal(os.Stderr.Fd()) && os.Getenv("TERM") != "dumb" {
		runMonitorUI(st, stop)
	} else {
		runMonitorPlain(ctx, st)
	}

	// The screen is gone; if the program is still up it was detached from, so
	// stop waiting politely.
	select {
	case code := <-done:
		fmt.Fprint(os.Stderr, monitorSummary(newStyles(), st.snapshot()))

		return code
	case <-time.After(3 * time.Second):
		if p := proc.Load(); p != nil {
			_ = p.Kill()
		}
	}

	code := <-done
	fmt.Fprint(os.Stderr, monitorSummary(newStyles(), st.snapshot()))

	return code
}

// runProgram starts the built binary and keeps the monitor state fed until it
// exits.
func runProgram(
	ctx context.Context, bin string, args []string,
	st *monitorState, opts *options, onStart func(*os.Process),
) (int, error) {
	//nolint:gosec // the binary we just built from the user's own package
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "GODEBUG="+godebugFor(os.Getenv("GODEBUG"), opts.goroutines))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("cannot start %s: %w", bin, err)
	}

	onStart(cmd.Process)

	st.mu.Lock()
	st.pid = cmd.Process.Pid
	st.pprofAddr = opts.pprof
	st.mu.Unlock()

	var streams sync.WaitGroup
	streams.Add(2)

	go func() { defer streams.Done(); readProgram(stdout, st, false) }()
	go func() { defer streams.Done(); readProgram(stderr, st, true) }()

	// Only one of the two can own the keyboard. With a screen up that is the
	// monitor, so the program is given an stdin that is simply closed.
	if opts.plain {
		go func() {
			_, _ = io.Copy(stdin, os.Stdin)
			_ = stdin.Close()
		}()
	} else {
		_ = stdin.Close()
	}

	go sampleLoop(ctx, cmd.Process.Pid, st)

	if opts.pprof != "" {
		go goroutineLoop(ctx, st, opts.pprof)
		go profileLoop(ctx, st, opts.pprof)
	}

	streams.Wait()

	code := waitExitCode(cmd)
	if code != 0 {
		st.setExited(fmt.Sprintf("exit status %d", code))
	} else {
		st.setExited("")
	}

	return code, nil
}

// godebugFor adds the traces the runtime tab lives on without discarding what
// the user already set.
//
// scheddetail is opt-in: it makes the runtime walk and print every goroutine
// under the scheduler lock on each interval, which is a real pause for a
// program with thousands of them. Without it, goroutine counts come from pprof.
func godebugFor(existing string, goroutines bool) string {
	wanted := []string{"gctrace=1", "schedtrace=1000", "inittrace=1"}
	if goroutines {
		wanted = []string{"gctrace=1", "schedtrace=2000", "scheddetail=1", "inittrace=1"}
	}

	for _, w := range wanted {
		key, _, _ := strings.Cut(w, "=")
		if strings.Contains(existing, key+"=") {
			continue
		}
		if existing != "" {
			existing += ","
		}
		existing += w
	}

	return existing
}

func readProgram(r io.Reader, st *monitorState, runtimeTraces bool) {
	br := bufio.NewReaderSize(r, 64*1024)

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if !runtimeTraces || !st.observeRuntimeLine(trimmed) {
				st.addOutput(trimmed)
			}
		}

		if err != nil {
			return
		}
	}
}

func sampleLoop(ctx context.Context, pid int, st *monitorState) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	prev := sampleProcess(ctx, pid)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := sampleProcess(ctx, pid)
			st.addSample(current, prev)

			if current.ok {
				prev = current
			}
		}
	}
}

// pprofURL normalises whatever the user typed into a base URL.
func pprofURL(addr string) string {
	return "http://" + strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
}

// goroutineLoop is deliberately separate from the profile loop: the goroutine
// profile is a cheap text fetch, while a cpu profile takes five seconds by
// definition. Sharing one loop would sample the goroutine count so rarely that
// a leak could not be told from a busy moment.
func goroutineLoop(ctx context.Context, st *monitorState, addr string) {
	base := pprofURL(addr)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		n, groups, err := goroutineProfile(ctx, base)

		st.mu.Lock()
		if err == nil {
			st.setGoroutinesLocked(n)
			st.stacks, st.pprofNote = groups, ""
		} else {
			st.pprofNote = err.Error()
		}
		st.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// profileLoop keeps the cpu and heap tables fresh. It shells out to
// `go tool pprof`, which is always present next to the go command we are
// already driving, so no profile format has to be understood here.
func profileLoop(ctx context.Context, st *monitorState, addr string) {
	base := pprofURL(addr)

	for {
		if out, err := pprofTop(ctx, "-sample_index=inuse_space", base+"/debug/pprof/heap"); err == nil {
			st.mu.Lock()
			st.heapProfile = out
			st.mu.Unlock()
		}

		if out, err := pprofTop(ctx, "", base+"/debug/pprof/profile?seconds=5"); err == nil {
			st.mu.Lock()
			st.cpuProfile = out
			st.mu.Unlock()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func pprofTop(ctx context.Context, extra, url string) (string, error) {
	args := []string{"tool", "pprof", "-top", "-nodecount=14"}
	if extra != "" {
		args = append(args, extra)
	}
	args = append(args, url)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "go", args...).Output()
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// stackGroup is a set of goroutines sitting in the same place. A leak is a
// group that keeps growing, which is why they are worth naming.
type stackGroup struct {
	count int
	where string
}

// parseGoroutineProfile reads the text goroutine profile:
//
//	goroutine profile: total 7
//	2 @ 0x1043 0x1099
//	#	0x1042	main.worker+0x1c	/app/main.go:12
//
// The label is the innermost frame that belongs to the program rather than to
// the runtime — "runtime.gopark" is where every blocked goroutine sits and says
// nothing about which of them is leaking.
func parseGoroutineProfile(body string) (total int, groups []stackGroup) {
	var current *stackGroup

	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "goroutine profile: total "):
			total, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "goroutine profile: total ")))

		case strings.HasPrefix(line, "#\t"):
			if current == nil || current.where != "" {
				continue
			}

			fields := strings.Split(line, "\t")
			if len(fields) < 3 {
				continue
			}

			fn, _, _ := strings.Cut(fields[2], "+")
			if fn != "" && !strings.HasPrefix(fn, "runtime.") && !strings.HasPrefix(fn, "runtime/") {
				current.where = fn
			}

		default:
			count, rest, ok := strings.Cut(strings.TrimSpace(line), " @ ")
			if !ok {
				continue
			}
			if _ = rest; count == "" {
				continue
			}

			n, err := strconv.Atoi(count)
			if err != nil {
				continue
			}

			groups = append(groups, stackGroup{count: n})
			current = &groups[len(groups)-1]
		}
	}

	for i := range groups {
		if groups[i].where == "" {
			groups[i].where = "(runtime)"
		}
	}

	sort.SliceStable(groups, func(i, j int) bool { return groups[i].count > groups[j].count })

	return total, groups
}

func goroutineProfile(ctx context.Context, base string) (int, []stackGroup, error) {
	body, err := fetchText(ctx, base+"/debug/pprof/goroutine?debug=1", 1<<20)
	if err != nil {
		return 0, nil, err
	}

	total, groups := parseGoroutineProfile(body)

	return total, groups, nil
}

func fetchText(ctx context.Context, url string, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pprof unreachable")
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func goroutineCount(ctx context.Context, base string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/debug/pprof/goroutine?debug=1", nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("pprof unreachable at %s", base)
	}
	defer func() { _ = resp.Body.Close() }()

	head := bufio.NewReader(io.LimitReader(resp.Body, 4096))

	line, err := head.ReadString('\n')
	if err != nil && line == "" {
		return 0, err
	}

	// "goroutine profile: total 37"
	if i := strings.LastIndex(line, " "); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(line[i+1:])); err == nil {
			return n, nil
		}
	}

	return 0, fmt.Errorf("unexpected goroutine profile header")
}
