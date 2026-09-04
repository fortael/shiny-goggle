package profiler

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// benchEnv turns the compiler phase breakdown off when set to "0". It exists
// because the measurement costs a temporary file per compiled package, and on a
// machine where that is expensive it should be possible to say no.
const benchEnv = "SHINY_GOGGLES_PHASES"

// compilePhases is what `compile -bench` reports about one package: where the
// time inside the compiler went, and how much code it dealt with.
type compilePhases struct {
	frontend time.Duration // parse, type check, inline, escape analysis
	backend  time.Duration // generate and write machine code
	lines    int
	funcs    int
}

func (p compilePhases) empty() bool { return p.frontend == 0 && p.backend == 0 && p.funcs == 0 }

// benchFileFor creates the file `compile -bench` should append to, and returns
// the flag to add and a function that reads and removes it. A nil reader means
// the measurement is unavailable and the compile must run unchanged.
func benchFileFor(tool string) (flag string, read func() compilePhases) {
	if tool != "compile" || os.Getenv(benchEnv) == "0" {
		return "", nil
	}

	f, err := os.CreateTemp("", "shiny-goggles-bench-*.txt")
	if err != nil {
		return "", nil
	}

	path := f.Name()
	_ = f.Close()

	return "-bench=" + path, func() compilePhases {
		defer func() { _ = os.Remove(path) }()

		file, err := os.Open(path) //nolint:gosec // a temp file we just created
		if err != nil {
			return compilePhases{}
		}
		defer func() { _ = file.Close() }()

		return parseCompileBench(file)
	}
}

// parseCompileBench reads the compiler's own benchmark output:
//
//	BenchmarkCompile:gopkg.in/yaml.v3:fe:parse         1  222436583 ns/op  27.85 %  11298 lines  50792 lines/s
//	BenchmarkCompile:gopkg.in/yaml.v3:fe:subtotal      1  271388250 ns/op  33.98 %
//	BenchmarkCompile:gopkg.in/yaml.v3:be:compilefuncs  1  501222625 ns/op  62.76 %    395 funcs    788 funcs/s
//	BenchmarkCompile:gopkg.in/yaml.v3:be:subtotal      1  527290583 ns/op  66.02 %
//
// Each half already reports its own subtotal, so only those two lines are read
// for the timings — adding the individual phases up would count them twice.
func parseCompileBench(r *os.File) compilePhases {
	var out compilePhases

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for sc.Scan() {
		name, rest, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || !strings.HasPrefix(name, "BenchmarkCompile:") {
			continue
		}

		section, phase := benchSection(name)
		fields := strings.Fields(rest)

		if phase == "subtotal" {
			switch section {
			case "fe":
				out.frontend = nanosecondsField(fields)
			case "be":
				out.backend = nanosecondsField(fields)
			}
		}

		if n, ok := countBefore(fields, "lines"); ok && n > out.lines {
			out.lines = n
		}
		if n, ok := countBefore(fields, "funcs"); ok && n > out.funcs {
			out.funcs = n
		}
	}

	return out
}

// benchSection splits "BenchmarkCompile:<pkg>:<fe|be>:<phase>" into its last two
// components. Import paths never contain a colon, so the tail is unambiguous.
func benchSection(name string) (section, phase string) {
	last := strings.LastIndex(name, ":")
	if last < 0 {
		return "", ""
	}

	phase = name[last+1:]

	if prev := strings.LastIndex(name[:last], ":"); prev >= 0 {
		section = name[prev+1 : last]
	}

	return section, phase
}

// nanosecondsField reads the "<n> ns/op" pair.
func nanosecondsField(fields []string) time.Duration {
	for i, f := range fields {
		if f == "ns/op" && i > 0 {
			if n, err := strconv.ParseInt(fields[i-1], 10, 64); err == nil {
				return time.Duration(n)
			}
		}
	}

	return 0
}

// countBefore reads the "<n> lines" / "<n> funcs" pairs, ignoring the rate
// columns that follow them ("8178 lines/s").
func countBefore(fields []string, unit string) (int, bool) {
	for i, f := range fields {
		if f == unit && i > 0 {
			if n, err := strconv.Atoi(fields[i-1]); err == nil {
				return n, true
			}
		}
	}

	return 0, false
}
