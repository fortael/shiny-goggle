package profiler

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector gathers what the event server hands back, from whichever goroutine
// happens to deliver it.
type collector struct {
	mu     sync.Mutex
	events []any
}

func (c *collector) handle(msg any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.events = append(c.events, msg)
}

// wait blocks until n events have arrived, so the tests never race the server.
func (c *collector) wait(t *testing.T, n int) []any {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.events)
		c.mu.Unlock()

		if got >= n {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.events) < n {
		t.Fatalf("got %d events, want %d: %+v", len(c.events), n, c.events)
	}

	return append([]any(nil), c.events...)
}

// startServer brings the event socket up and points the shim side at it. These
// tests cannot run in parallel: the socket path is derived from the pid, so a
// second server in the same process would take the first one's path.
func startServer(t *testing.T) *collector {
	t.Helper()

	c := &collector{}

	srv, err := listenEvents(c.handle)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(srv.close)
	t.Setenv(sockEnv, srv.path)

	return c
}

func TestEventRoundTrip(t *testing.T) {
	c := startServer(t)

	client := dialEvents()
	if client == nil {
		t.Fatal("could not reach the event socket")
	}

	client.send(wireEvent{Kind: kindStart, Tool: "compile", Pkg: "example.com/app", Std: true})
	client.send(wireEvent{
		Kind: kindEnd, Tool: "compile", Pkg: "example.com/app",
		Dur: 1.5, Fail: true, Fe: 0.25, Be: 0.75, Lines: 120, Funcs: 9,
	})
	client.close()

	events := c.wait(t, 2)

	start, ok := events[0].(startEvent)
	if !ok {
		t.Fatalf("first event is %T", events[0])
	}
	if start.tool != "compile" || start.pkg != "example.com/app" || !start.std {
		t.Fatalf("%+v", start)
	}

	end, ok := events[1].(endEvent)
	if !ok {
		t.Fatalf("second event is %T", events[1])
	}
	if end.id != start.id {
		t.Fatalf("start id %d and end id %d differ; they share a connection", start.id, end.id)
	}
	if end.dur != 1500*time.Millisecond || !end.fail || end.aborted {
		t.Fatalf("%+v", end)
	}
	if end.frontend != 250*time.Millisecond || end.backend != 750*time.Millisecond ||
		end.lines != 120 || end.funcs != 9 {
		t.Fatalf("compiler phases lost in transit: %+v", end)
	}
}

// A tool that is killed never sends its end event. Without this the package
// would sit in the running list for the rest of the build.
func TestEventAbortedWhenConnectionDropsMidAction(t *testing.T) {
	c := startServer(t)

	client := dialEvents()
	if client == nil {
		t.Fatal("could not reach the event socket")
	}

	client.send(wireEvent{Kind: kindStart, Tool: "compile", Pkg: "example.com/app"})
	client.close()

	events := c.wait(t, 2)

	end, ok := events[1].(endEvent)
	if !ok || !end.aborted {
		t.Fatalf("want an aborted end event, got %+v", events[1])
	}
}

func TestEventIdsAreOnePerConnection(t *testing.T) {
	c := startServer(t)

	for range 3 {
		client := dialEvents()
		if client == nil {
			t.Fatal("could not reach the event socket")
		}

		client.send(wireEvent{Kind: kindStart, Tool: "compile", Pkg: "p"})
		client.send(wireEvent{Kind: kindEnd, Tool: "compile", Pkg: "p", Dur: 0.1})
		client.close()
	}

	seen := map[uint64]bool{}
	for _, ev := range c.wait(t, 6) {
		if s, ok := ev.(startEvent); ok {
			if seen[s.id] {
				t.Fatalf("id %d handed out twice", s.id)
			}
			seen[s.id] = true
		}
	}

	if len(seen) != 3 {
		t.Fatalf("got %d distinct ids, want 3", len(seen))
	}
}

// With no driver listening the shim must degrade to a plain exec wrapper rather
// than fail the build, which means every client method tolerates a nil receiver.
func TestEventClientIsHarmlessWithoutADriver(t *testing.T) {
	t.Setenv(sockEnv, "")

	client := dialEvents()
	if client != nil {
		t.Fatal("expected no client without a socket")
	}

	client.send(wireEvent{Kind: kindStart})
	client.close()
}

func TestPumpHoldsOutputUntilReleased(t *testing.T) {
	t.Parallel()

	var dst strings.Builder

	p := newPump(&dst)

	p.write([]byte("first "))
	p.write([]byte("second "))

	if dst.String() != "" {
		t.Fatalf("wrote through while the screen was up: %q", dst.String())
	}

	p.release()

	if dst.String() != "first second " {
		t.Fatalf("got %q", dst.String())
	}

	// Everything after the handover goes straight to the terminal.
	p.write([]byte("third"))

	if dst.String() != "first second third" {
		t.Fatalf("got %q", dst.String())
	}
}

// flush replays the diagnostics under the frozen screen but keeps the pump
// buffered, so the report can still be printed before the program's own output.
func TestPumpFlushDoesNotHandOver(t *testing.T) {
	t.Parallel()

	var dst strings.Builder

	p := newPump(&dst)
	p.write([]byte("compiler said this\n"))
	p.flush()

	if dst.String() != "compiler said this\n" {
		t.Fatalf("got %q", dst.String())
	}

	p.write([]byte("still buffered"))

	if dst.String() != "compiler said this\n" {
		t.Fatalf("flush handed over too early: %q", dst.String())
	}
}

func TestCopyLinesForwardsVerbatimAndReportsEachLine(t *testing.T) {
	t.Parallel()

	var dst strings.Builder

	p := newPump(&dst)
	p.release()

	var seen []string

	// The last line has no newline: it must still reach both destinations.
	copyLines(strings.NewReader("one\ntwo\r\nthree"), p, func(l string) { seen = append(seen, l) })

	if got := strings.Join(seen, "|"); got != "one|two|three" {
		t.Fatalf("line callback got %q, want the line endings stripped", got)
	}

	if dst.String() != "one\ntwo\r\nthree" {
		t.Fatalf("stream was not forwarded verbatim: %q", dst.String())
	}
}

// The first byte on stdout is what tells the driver that `go run` has stopped
// building and the program has started talking.
func TestCopyRawSignalsTheFirstByteOnce(t *testing.T) {
	t.Parallel()

	var dst strings.Builder

	p := newPump(&dst)
	p.release()

	calls := 0

	copyRaw(strings.NewReader("no trailing newline here"), p, func() { calls++ })

	if calls != 1 {
		t.Fatalf("first byte reported %d times, want once", calls)
	}
	if dst.String() != "no trailing newline here" {
		t.Fatalf("got %q", dst.String())
	}
}

func TestCopyRawStaysSilentOnAnEmptyStream(t *testing.T) {
	t.Parallel()

	calls := 0

	copyRaw(strings.NewReader(""), newPump(io.Discard), func() { calls++ })

	if calls != 0 {
		t.Fatal("a program that printed nothing must not look like it started")
	}
}

// cacheHome points os.UserCacheDir at a temporary directory on both platforms:
// darwin derives it from HOME, linux from XDG_CACHE_HOME.
func cacheHome(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

func TestHistoryRoundTrip(t *testing.T) {
	cacheHome(t)

	const key = "go build ./..."

	h := loadHistory("/some/project")
	if got := h.eta(key); got != 0 {
		t.Fatalf("a fresh cache predicted %v", got)
	}

	sn := snap{
		slow: []*action{
			{tool: "compile", pkg: "example.com/slow", dur: 2 * time.Second},
			{tool: "compile", pkg: "example.com/quick", dur: 100 * time.Millisecond},
		},
	}
	h.record(key, 30*time.Second, sn)

	// A different project must not see it.
	if other := loadHistory("/another/project"); other.eta(key) != 0 {
		t.Fatal("history leaked between projects")
	}

	reloaded := loadHistory("/some/project")
	if got := reloaded.eta(key); got != 30*time.Second {
		t.Fatalf("eta=%v, want the recorded 30s", got)
	}

	expect := reloaded.expectations()
	if expect["example.com/slow"] != 2*time.Second {
		t.Fatalf("per package timings lost: %+v", expect)
	}
}

func TestHistoryETAIsTheMedianOfRecentRuns(t *testing.T) {
	t.Parallel()

	h := &history{Commands: map[string][]float64{}, Packages: map[string]float64{}}

	// One cold outlier must not drag the estimate along with it.
	h.Commands["k"] = []float64{4, 5, 60, 6, 5}

	if got := h.eta("k"); got != 5*time.Second {
		t.Fatalf("eta=%v, want the median 5s", got)
	}
}

func TestHistoryKeepsOnlyRecentRuns(t *testing.T) {
	cacheHome(t)

	h := loadHistory("/p")
	for i := range keptDurations + 5 {
		h.record("k", time.Duration(i+1)*time.Second, snap{})
	}

	if got := len(h.Commands["k"]); got != keptDurations {
		t.Fatalf("kept %d durations, want %d", got, keptDurations)
	}
	// The window slides, so the oldest run is gone and the newest is present.
	if h.Commands["k"][len(h.Commands["k"])-1] != float64(keptDurations+5) {
		t.Fatalf("newest run missing: %v", h.Commands["k"])
	}
}

// Only compiling counts, and only when it worked: a failed action's duration
// says nothing about how long the package usually takes.
func TestHistoryMergesOnlySuccessfulCompiles(t *testing.T) {
	t.Parallel()

	h := &history{Commands: map[string][]float64{}, Packages: map[string]float64{}}

	h.mergePackage(&action{tool: "compile", pkg: "ok", dur: time.Second})
	h.mergePackage(&action{tool: "compile", pkg: "failed", dur: time.Second, failed: true})
	h.mergePackage(&action{tool: "link", pkg: "linked", dur: time.Second})
	h.mergePackage(&action{tool: "compile", pkg: "", dur: time.Second})

	if len(h.Packages) != 1 || h.Packages["ok"] != 1 {
		t.Fatalf("%+v", h.Packages)
	}

	// A second observation is smoothed rather than replacing the first.
	h.mergePackage(&action{tool: "compile", pkg: "ok", dur: 2 * time.Second})

	if got := h.Packages["ok"]; got <= 1 || got >= 2 {
		t.Fatalf("got %v, want a value between the two observations", got)
	}
}

func TestHistoryPruneKeepsTheSlowestPackages(t *testing.T) {
	t.Parallel()

	h := &history{Commands: map[string][]float64{}, Packages: map[string]float64{}}
	for i := range maxHistoryPackages + 100 {
		h.Packages[fmt.Sprintf("pkg%d", i)] = float64(i)
	}

	h.prune()

	if len(h.Packages) != maxHistoryPackages {
		t.Fatalf("kept %d packages, want %d", len(h.Packages), maxHistoryPackages)
	}
	// The cheap ones are the ones worth forgetting; the slow ones are the whole
	// point of the cache.
	if _, ok := h.Packages["pkg0"]; ok {
		t.Fatal("the fastest package survived the prune")
	}
	if _, ok := h.Packages[fmt.Sprintf("pkg%d", maxHistoryPackages+99)]; !ok {
		t.Fatal("the slowest package was pruned")
	}
}

// fakeTool writes the arguments it was given to a file and exits with the code
// its name asks for, which is enough to observe what the shim does to a real
// tool invocation.
func fakeTool(t *testing.T, name string, exitCode int) (path, argsFile string) {
	t.Helper()

	dir := t.TempDir()
	path = filepath.Join(dir, name)
	argsFile = filepath.Join(dir, "args.txt")

	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit %d\n", argsFile, exitCode)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	return path, argsFile
}

func toolArgs(t *testing.T, argsFile string) []string {
	t.Helper()

	data, err := os.ReadFile(argsFile) //nolint:gosec // written by the fixture above
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// The go command asks every tool for its identity to build cache keys. Reporting
// those probes would be noise, and touching them would put the profiler into the
// hash and split the build cache in two.
func TestToolexecPassesVersionProbesThroughUntouched(t *testing.T) {
	c := startServer(t)

	tool, argsFile := fakeTool(t, "compile", 0)

	if code := runToolexec([]string{tool, "-V=full"}); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	if got := toolArgs(t, argsFile); strings.Join(got, " ") != "-V=full" {
		t.Fatalf("the probe was modified: %v", got)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.events) != 0 {
		t.Fatalf("a version probe was reported as work: %+v", c.events)
	}
}

// -bench is added by the shim rather than through -gcflags, and the compiler
// takes flags only before the source file list. Appending it would make every
// build fail with the flag read as a file name.
func TestToolexecInsertsBenchFlagBeforeTheSourceFiles(t *testing.T) {
	startServer(t)

	tool, argsFile := fakeTool(t, "compile", 0)

	runToolexec([]string{tool, "-o", "pkg.a", "-p", "example.com/app", "a.go", "b.go"})

	got := toolArgs(t, argsFile)

	bench, firstFile := -1, -1

	for i, a := range got {
		if strings.HasPrefix(a, "-bench=") && bench < 0 {
			bench = i
		}
		if strings.HasSuffix(a, ".go") && firstFile < 0 {
			firstFile = i
		}
	}

	if bench < 0 {
		t.Fatalf("no -bench flag was added: %v", got)
	}
	if firstFile < 0 {
		t.Fatalf("the source files vanished: %v", got)
	}
	if bench > firstFile {
		t.Fatalf("-bench lands after the source files, which would break the build: %v", got)
	}

	// The temporary file it points at must not be left behind.
	path := strings.TrimPrefix(got[bench], "-bench=")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s was not cleaned up", path)
	}
}

func TestToolexecReportsPackageAndFailure(t *testing.T) {
	c := startServer(t)
	t.Setenv("TOOLEXEC_IMPORTPATH", "example.com/app")

	tool, _ := fakeTool(t, "link", 3)

	if code := runToolexec([]string{tool, "-o", "bin"}); code != 3 {
		t.Fatalf("exit code %d, want the tool's own 3", code)
	}

	events := c.wait(t, 2)

	start, ok := events[0].(startEvent)
	if !ok || start.tool != "link" || start.pkg != "example.com/app" {
		t.Fatalf("%+v", events[0])
	}

	end, ok := events[1].(endEvent)
	if !ok || !end.fail {
		t.Fatalf("a failing tool was not reported as failed: %+v", events[1])
	}
}

// Nothing about the profiler may stop a build, so a tool that cannot be found
// still produces an exit code rather than a panic.
func TestToolexecSurvivesAMissingTool(t *testing.T) {
	startServer(t)

	if code := runToolexec([]string{filepath.Join(t.TempDir(), "not-here"), "-o", "x"}); code == 0 {
		t.Fatal("a missing tool should not report success")
	}
}

func TestBenchFileOnlyForTheCompiler(t *testing.T) {
	t.Parallel()

	if flag, read := benchFileFor("link"); read != nil || flag != "" {
		t.Fatal("only the compiler understands -bench")
	}

	flag, read := benchFileFor("compile")
	if read == nil {
		t.Fatal("expected a bench file for the compiler")
	}

	path := strings.TrimPrefix(flag, "-bench=")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the bench file was not created: %v", err)
	}

	if phases := read(); !phases.empty() {
		t.Fatalf("an empty bench file produced %+v", phases)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("reading the phases must also remove the file")
	}
}

func TestBenchFileCanBeTurnedOff(t *testing.T) {
	t.Setenv(benchEnv, "0")

	if _, read := benchFileFor("compile"); read != nil {
		t.Fatalf("%s=0 must disable the measurement", benchEnv)
	}
}
