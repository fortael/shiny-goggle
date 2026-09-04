package profiler

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// procSample is what the operating system will tell us about any process,
// without the program having to cooperate in any way.
type procSample struct {
	rssKB   float64
	cpuTime time.Duration // total cpu consumed since the process started
	at      time.Time
	ok      bool
}

// cpuPercentSince turns two cumulative readings into the load right now. ps
// reports %CPU as an average over the whole life of the process, which is the
// wrong question for a live screen: a service that was busy at startup would
// look busy forever.
func (s procSample) cpuPercentSince(prev procSample) (float64, bool) {
	if !s.ok || !prev.ok {
		return 0, false
	}

	window := s.at.Sub(prev.at)
	if window <= 0 || s.cpuTime < prev.cpuTime {
		return 0, false
	}

	return (s.cpuTime - prev.cpuTime).Seconds() / window.Seconds() * 100, true
}

// sampleProcess asks ps about a pid. It is deliberately the lowest common
// denominator: /proc does not exist on darwin and parsing mach task info would
// buy nothing here, one cheap process per second is fine.
func sampleProcess(ctx context.Context, pid int) procSample {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=,time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return procSample{}
	}

	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return procSample{}
	}

	rss, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return procSample{}
	}

	cpu, ok := parseCPUTime(fields[1])
	if !ok {
		return procSample{}
	}

	return procSample{rssKB: rss, cpuTime: cpu, at: time.Now(), ok: true}
}

// parseCPUTime reads the cumulative cpu time ps prints, in any of the shapes it
// uses as the number grows: "12.34", "01:23.45", "1:02:03" or "2-01:02:03".
func parseCPUTime(s string) (time.Duration, bool) {
	days := 0.0

	if before, after, ok := strings.Cut(s, "-"); ok {
		d, err := strconv.ParseFloat(before, 64)
		if err != nil {
			return 0, false
		}
		days, s = d, after
	}

	var total float64

	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + v
	}

	total += days * 24 * 60 * 60

	return time.Duration(total * float64(time.Second)), true
}

// gcEvent is one line of GODEBUG=gctrace=1, which every Go program emits
// without being rebuilt for it.
//
//	gc 1 @0.012s 0%: 0.018+0.34+0.003 ms clock, 0.14+0.10/0.31/0.53+0.028 ms cpu,
//	4->4->1 MB, 5 MB goal, 0 MB stacks, 0 MB globals, 8 P
type gcEvent struct {
	num      int
	at       time.Duration
	gcCPU    float64       // percent of total CPU spent in GC since start
	pause    time.Duration // the two stop-the-world phases
	heapPrev float64       // MB at the start of the cycle
	heapLive float64       // MB still live after it
	goal     float64       // MB the next cycle is aimed at
}

func parseGCTrace(line string) (gcEvent, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "gc ") {
		return gcEvent{}, false
	}

	head, rest, ok := strings.Cut(line, ":")
	if !ok {
		return gcEvent{}, false
	}

	fields := strings.Fields(head)
	if len(fields) < 4 {
		return gcEvent{}, false
	}

	var ev gcEvent

	ev.num, _ = strconv.Atoi(fields[1])
	ev.at = parseSeconds(strings.TrimPrefix(fields[2], "@"))
	ev.gcCPU, _ = strconv.ParseFloat(strings.TrimSuffix(fields[3], "%"), 64)

	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)

		switch {
		case strings.HasSuffix(part, "ms clock"):
			ev.pause = stopTheWorld(strings.TrimSuffix(part, " ms clock"))
		case strings.HasSuffix(part, "MB goal"):
			ev.goal, _ = strconv.ParseFloat(strings.TrimSuffix(part, " MB goal"), 64)
		case strings.HasSuffix(part, "MB") && strings.Contains(part, "->"):
			ev.heapPrev, ev.heapLive = heapTransition(strings.TrimSuffix(part, " MB"))
		}
	}

	return ev, true
}

// stopTheWorld adds the first and last clock phases, the ones that actually
// stop the program; the middle phase runs concurrently with it.
func stopTheWorld(clock string) time.Duration {
	parts := strings.Split(clock, "+")
	if len(parts) < 3 {
		return 0
	}

	first, _ := strconv.ParseFloat(parts[0], 64)
	last, _ := strconv.ParseFloat(parts[len(parts)-1], 64)

	return time.Duration((first + last) * float64(time.Millisecond))
}

// heapTransition reads "4->5->2": heap at the start, at the end, and still live.
func heapTransition(s string) (before, live float64) {
	parts := strings.Split(s, "->")
	if len(parts) != 3 {
		return 0, 0
	}

	before, _ = strconv.ParseFloat(parts[0], 64)
	live, _ = strconv.ParseFloat(parts[2], 64)

	return before, live
}

func parseSeconds(s string) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil {
		return 0
	}

	return time.Duration(secs * float64(time.Second))
}

// initEvent is one line of GODEBUG=inittrace=1, printed once per package as the
// program starts:
//
//	init internal/godebug @0.43 ms, 0.22 ms clock, 2176 bytes, 44 allocs
type initEvent struct {
	pkg    string
	at     time.Duration
	clock  time.Duration
	bytes  int64
	allocs int64
}

func parseInitTrace(line string) (initEvent, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "init ") {
		return initEvent{}, false
	}

	head, rest, ok := strings.Cut(line, "@")
	if !ok {
		return initEvent{}, false
	}

	ev := initEvent{pkg: strings.TrimSpace(strings.TrimPrefix(head, "init "))}
	if ev.pkg == "" {
		return initEvent{}, false
	}

	for i, part := range strings.Split(rest, ",") {
		fields := strings.Fields(part)
		if len(fields) < 2 {
			return initEvent{}, false
		}

		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return initEvent{}, false
		}

		switch {
		case i == 0: // "0.43 ms" — when the init started
			ev.at = time.Duration(value * float64(time.Millisecond))
		case fields[len(fields)-1] == "clock":
			ev.clock = time.Duration(value * float64(time.Millisecond))
		case fields[1] == "bytes":
			ev.bytes = int64(value)
		case fields[1] == "allocs":
			ev.allocs = int64(value)
		}
	}

	return ev, true
}

// parseSchedGoroutine reads one goroutine line of GODEBUG=scheddetail=1:
//
//	G12: status=1(chan receive) m=nil lockedm=nil
//
// It is the only way to count goroutines in a program that does not serve
// pprof, and the wait reason in brackets is what tells a leak apart from work.
func parseSchedGoroutine(line string) (reason string, ok bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != 'G' || !strings.Contains(line, "status=") {
		return "", false
	}

	if _, err := strconv.Atoi(strings.TrimSuffix(strings.Fields(line)[0][1:], ":")); err != nil {
		return "", false
	}

	// status=4(GC worker (idle)) m=nil — the reason itself may contain
	// brackets, so take everything between the first and the last one, after
	// trimming the fields that follow it.
	status := line
	if before, _, ok := strings.Cut(line, " m="); ok {
		status = before
	}

	open := strings.Index(status, "(")
	closed := strings.LastIndex(status, ")")

	if open < 0 || closed <= open+1 {
		return "", true
	}

	return status[open+1 : closed], true
}

// schedSample is one line of GODEBUG=schedtrace=1000.
//
//	SCHED 1004ms: gomaxprocs=8 idleprocs=7 threads=6 spinningthreads=0 … runqueue=0 [0 0 0]
type schedSample struct {
	threads  int
	procs    int
	idle     int
	runqueue int
}

func parseSchedTrace(line string) (schedSample, bool) {
	if !strings.HasPrefix(strings.TrimSpace(line), "SCHED ") {
		return schedSample{}, false
	}

	var s schedSample

	for _, f := range strings.Fields(line) {
		key, value, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}

		n, err := strconv.Atoi(value)
		if err != nil {
			continue
		}

		switch key {
		case "gomaxprocs":
			s.procs = n
		case "idleprocs":
			s.idle = n
		case "threads":
			s.threads = n
		case "runqueue":
			s.runqueue = n
		}
	}

	return s, s.procs > 0
}
