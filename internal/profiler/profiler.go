// Package profiler turns any `go build` / `go run` / `go test` into a live
// compilation profiler. It attaches itself to the go command through -toolexec,
// collects an event for every tool invocation and renders what is happening.
//
// The same binary is both the driver and the toolexec shim: the go command
// re-executes it as `shiny-goggles _toolexec <realtool> <args...>`, in which case it
// only reports timings and execs the real tool.
package profiler

import (
	"fmt"
	"os"
	"path/filepath"
)

// Mode selects how much of the build is put on the screen.
type Mode int

const (
	// ModeSimple shows a spinner, a progress bar, what is compiling right now
	// and the elapsed time. Nothing is recorded, nothing is reported afterwards.
	ModeSimple Mode = iota
	// ModeVerbose shows the full screen: running packages with their usual
	// duration, the slowest packages, what blocked the build, a trace-viewer
	// style timeline, and a report once it is over.
	ModeVerbose
	// ModeMonitor builds like ModeVerbose and then stays with the program:
	// memory, garbage collection, cpu and its output on separate tabs.
	ModeMonitor
)

// toolexecMarker is the first argument the go command passes back to us when we
// are invoked as a -toolexec wrapper.
const toolexecMarker = "_toolexec"

// Main is the entry point of the single binary.
func Main() int {
	if args, ok := toolexecArgs(os.Args); ok {
		return runToolexec(args)
	}

	return runDriver(os.Args[1:])
}

// toolexecArgs decides whether this process was started by the go command as a
// tool wrapper and returns the real tool invocation (tool path + arguments).
//
// Two shapes are recognised:
//   - "shiny-goggles _toolexec /path/to/compile ..." — what we ask for;
//   - "shiny-goggles /path/to/compile ..."           — a bare -toolexec=shiny-goggles setup,
//     for anyone who wires the binary into GOFLAGS by hand.
func toolexecArgs(argv []string) ([]string, bool) {
	if len(argv) < 2 {
		return nil, false
	}

	if argv[1] == toolexecMarker {
		if len(argv) < 3 {
			return nil, false
		}

		return argv[2:], true
	}

	if !filepath.IsAbs(argv[1]) {
		return nil, false
	}

	info, err := os.Stat(argv[1])
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return nil, false
	}

	return argv[1:], true
}

func usage(w *os.File) {
	fmt.Fprint(w, `shiny-goggles — see what the go command is actually doing

usage:
  shiny-goggles [flags] [--] <go command>          # any path to go works
  shiny-goggles [flags] [--] <subcommand> [args]   # "go" is implied

Anything that is not the go command is refused rather than run unprofiled.

examples:
  shiny-goggles build ./...
  shiny-goggles -verbose test ./internal/...
  shiny-goggles -monitor -pprof=localhost:6060 run . serve
  shiny-goggles -verbose -trace=trace.json build ./...
  shiny-goggles /opt/go1.25/bin/go build ./...

screens:
  -verbose          slowest packages, what blocked the build, a timeline, and a
                    report with the critical path. The screen is kept afterwards
  -monitor          build, then stay with the running program: memory, garbage
                    collection, cpu and its output on tabs. Needs "go run"; the
                    program is not modified in any way
  -clear            erase the verbose screen once the build ends and print a one
                    line summary, so only the program is left on the terminal
  -plain            never take over the terminal, print plain progress lines

The progress bar is honest: "go build -n" is asked, next to the real build, what
it is about to compile, and cached packages are simply not in that answer.

flags:
  -top N            rows reserved for the "slowest" chart, 5..10 (default 10)
  -blocking N       rows reserved for the "blocking" table (default 5) — the
                    packages that compiled alone while everything else waited
  -recent N         rows reserved for the "done" list (default 5)
  -hide-std         leave standard library packages out of the lists
  -ignore PATTERN   keep packages out of the lists, charts, timeline and trace.
                    "*" spans slashes, so "example.com/org/*" covers a tree; a
                    pattern without one names a package and everything under it.
                    Repeatable, or comma separated, or set SHINY_GOGGLES_IGNORE
                    to keep it off a command line that is being recorded.
                    Ignored packages are still built and still counted in the
                    totals — only their names are held back
  -trace FILE       write a trace of the build — one track per parallel worker,
                    one slice per package — and open it at https://ui.perfetto.dev
  -go-trace FILE    the go command's own -debug-trace instead. Note that perfetto
                    reports thousands of FLOW_NO_ENCLOSING_SLICE errors on it,
                    and with "go run" it keeps recording while the program runs
  -actiongraph FILE dump the build action graph (-debug-actiongraph)
  -pprof ADDR       in monitor mode, read live profiles from a program that
                    serves net/http/pprof, e.g. -pprof=localhost:6060. Adds the
                    goroutine count, what they are blocked on, and cpu and heap
                    tables from "go tool pprof"
  -goroutines       count goroutines without pprof, from GODEBUG=scheddetail.
                    The runtime then walks and prints every goroutine under the
                    scheduler lock twice a second, which is not free for a
                    program that has thousands of them
  -no-history       do not read or write the build duration cache
  -h, -help         this message

keys (verbose):
  q, ctrl+c         stop the build
  v                 show or hide standard library packages in the lists

keys (monitor):
  q, ctrl+c         ask the program to stop; again to stop waiting
  tab, 1, 2, 3      switch between runtime, output and profile
  ↑ ↓ pgup pgdn g   scroll the output, g goes back to following it
`)
}
