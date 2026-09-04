package profiler

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

type options struct {
	mode        Mode
	top         int
	recent      int
	blocking    int
	hideStd     bool
	plain       bool
	trace       string // our own trace of the build
	goTrace     string // the go command's -debug-trace, warts and all
	actiongraph string
	graphTemp   bool // the action graph is ours to read and delete
	testing     bool // the command under way is `go test`
	pprof       string
	clear       bool // erase the verbose screen when the build ends
	ignore      ignoreList
	goroutines  bool // count goroutines without pprof, via scheddetail
	noHistory   bool
}

// verbose covers the monitor too: before it can watch a program run, it builds
// one, and there is no reason to be quieter about that than -verbose is.
func (o *options) verbose() bool { return o.mode == ModeVerbose || o.mode == ModeMonitor }

// toolexecSubcommands are the go subcommands that accept build flags, i.e. the
// ones we can instrument.
var toolexecSubcommands = map[string]bool{
	"build":    true,
	"install":  true,
	"run":      true,
	"test":     true,
	"vet":      true,
	"generate": true,
	"list":     true,
}

// goSubcommands is every subcommand the go command has. It is wider than
// toolexecSubcommands on purpose: `shiny-goggles mod tidy` has nothing to
// profile, but it is still a go command and should run rather than be refused.
var goSubcommands = map[string]bool{
	"bug": true, "build": true, "clean": true, "doc": true, "env": true,
	"fix": true, "fmt": true, "generate": true, "get": true, "install": true,
	"list": true, "mod": true, "run": true, "telemetry": true, "test": true,
	"tool": true, "version": true, "vet": true, "work": true,
}

// goBinary matches the name of a go command: "go" itself, the versioned
// wrappers that `go install golang.org/dl/go1.25.1@latest` produces, and the
// Windows spelling of either.
var goBinary = regexp.MustCompile(`^go[0-9.]*(\.exe)?$`)

// isGoCommand looks at the base name, so any path to a go binary is accepted:
// "go", "/opt/go1.25/bin/go", "./go1.24.3".
func isGoCommand(arg string) bool {
	return goBinary.MatchString(filepath.Base(arg))
}

func runDriver(argv []string) int {
	opts, rest, err := parseArgs(argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)

			return 0
		}

		fmt.Fprintf(os.Stderr, "shiny-goggles: %v\n\n", err)
		usage(os.Stderr)

		return 2
	}

	if len(rest) == 0 {
		usage(os.Stderr)

		return 2
	}

	goArgs, err := normalizeGoCommand(rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shiny-goggles: %v\n\n", err)
		usage(os.Stderr)

		return 2
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shiny-goggles: cannot locate own binary (%v), running unprofiled\n", err)

		return execPassthrough(goArgs)
	}

	// The action graph is the only source for the critical path and for what
	// was served from the cache, so ask for it even when the user did not.
	if opts.actiongraph == "" && opts.verbose() {
		if f, err := os.CreateTemp("", "shiny-goggles-graph-*.json"); err == nil {
			opts.actiongraph, opts.graphTemp = f.Name(), true
			_ = f.Close()
		}
	}

	if opts.mode == ModeMonitor {
		return runMonitor(goArgs, self, opts)
	}

	full, sub, instrumented := injectToolexec(goArgs, self, opts)
	opts.testing = sub == "test"
	if !instrumented {
		return execPassthrough(full)
	}

	return runInstrumented(full, goArgs, sub, opts)
}

//nolint:gocyclo,funlen // this is the orchestration seam: process, sockets, UI and streams meet here
func runInstrumented(full, goArgs []string, sub string, opts *options) int {
	cwd, _ := os.Getwd()

	// The simple mode records nothing: no cache is read, no cache is written.
	hist := &history{Commands: map[string][]float64{}, Packages: map[string]float64{}}
	if opts.verbose() && !opts.noHistory {
		hist = loadHistory(cwd)
	}

	key := commandKey(goArgs)
	// Writing a trace needs every action, even when the screen would not.
	st := newStats(hist.expectations(), opts.verbose() || opts.trace != "", opts.ignore)

	// finish() is the single "the build part is over, give the terminal back"
	// switch. It can be pulled by the linker finishing, by the built program
	// writing its first byte, by the go command exiting or by the user.
	var (
		finishOnce sync.Once
		finished   = make(chan struct{})
	)
	finish := func() {
		finishOnce.Do(func() {
			st.finish()
			close(finished)
		})
	}

	srv, err := listenEvents(func(msg any) {
		switch ev := msg.(type) {
		case startEvent:
			st.start(ev)
		case endEvent:
			tool, ok := st.end(ev)
			// `go run` links and then immediately executes the binary: that is
			// where the build ends and the program's own output begins.
			if ok && tool == "link" && sub == "run" {
				time.AfterFunc(150*time.Millisecond, finish)
			}
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "shiny-goggles: %v, running unprofiled\n", err)

		return execPassthrough(goArgs)
	}
	defer srv.close()

	useTUI := !opts.plain && term.IsTerminal(os.Stderr.Fd()) && os.Getenv("TERM") != "dumb"

	outPump, errPump := newPump(os.Stdout), newPump(os.Stderr)
	if !useTUI {
		// Nothing owns the terminal, so never hold output back.
		outPump.release()
		errPump.release()
	}

	//nolint:gosec // the command line is what the user asked us to run
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Env = append(os.Environ(), sockEnv+"="+srv.path)
	cmd.Dir = cwd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(err)
	}

	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("cannot start %s: %w", full[0], err))
	}

	var streams sync.WaitGroup
	streams.Add(2)

	// `go test` prints results as packages finish while others still compile:
	// staying on screen and showing them is far more useful than bailing out on
	// the first byte the way a plain build does.
	go func() {
		defer streams.Done()

		if opts.testing {
			copyLines(stdout, outPump, st.observeTestLine)

			return
		}

		copyRaw(stdout, outPump, finish)
	}()
	go func() {
		defer streams.Done()
		copyLines(stderr, errPump, st.addDiag)
	}()

	// The go command exiting always ends the build phase, whatever else
	// happens. Reaping it here — after the pipes are drained — means a plain
	// `go build` already knows its exit status by the time we draw the summary.
	var exitCode int
	reaped := make(chan struct{})

	go func() {
		streams.Wait()
		exitCode = waitExitCode(cmd)
		st.setExit(exitCode)
		close(reaped)
		finish()
	}()

	// Ask the go command what it is about to compile; that count is the only
	// honest denominator for a progress bar. It runs beside the build, so it
	// costs no latency.
	planCtx, cancelPlan := context.WithCancel(context.Background())
	defer cancelPlan()

	go planPackages(planCtx, goArgs, st.addPlan)

	interrupts := 0
	interrupt := func() {
		interrupts++
		sig := os.Signal(os.Interrupt)
		if interrupts > 1 {
			sig = os.Kill
		}
		_ = cmd.Process.Signal(sig)
	}

	if useTUI {
		runTUI(st, opts, hist.eta(key), goArgs, finished, interrupt, cmd)
	} else {
		runPlain(st, opts, finished)
	}

	cancelPlan() // nothing left to draw a bar for

	sn := st.snapshot()

	var graph *buildGraph

	if opts.actiongraph != "" {
		if g, err := loadGraph(opts.actiongraph, opts.ignore); err == nil {
			graph = g
		}
		if opts.graphTemp {
			_ = os.Remove(opts.actiongraph)
			opts.actiongraph = "" // nothing to advertise, it was ours
		}
	}

	if opts.trace != "" {
		if err := writeTrace(opts.trace, strings.Join(goArgs, " "), sn); err != nil {
			fmt.Fprintf(os.Stderr, "shiny-goggles: %v\n", err)

			opts.trace = "" // do not advertise a file that is not there
		}
	}

	// Give the terminal back: replay everything the go command said, add the
	// report below the frozen screen, then let the program stream through.
	errPump.flush()
	fmt.Fprint(os.Stderr, renderSummary(sn, graph, opts, terminalWidth()))
	errPump.release()
	outPump.release()

	// The build is over, so stdin belongs to the program from now on — but only
	// if there is one. A `go build` has no use for keystrokes, and a reader
	// left on stdin would steal them from whatever owns the terminal next.
	if sub == "run" || sub == "test" {
		go func() {
			_, _ = io.Copy(stdin, os.Stdin)
			_ = stdin.Close()
		}()
	} else {
		_ = stdin.Close()
	}

	<-reaped

	// The estimate describes the build, not the program: `go run` of a service
	// that exits non-zero still tells us how long compiling it takes.
	if opts.verbose() && !opts.noHistory && sn.failed == 0 && sn.totalPackages() > 0 {
		hist.record(key, sn.elapsed, sn)
	}

	return exitCode
}

func waitExitCode(cmd *exec.Cmd) int {
	if err := cmd.Wait(); err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}

		return 1
	}

	return 0
}

func runTUI(
	st *stats,
	opts *options,
	eta time.Duration,
	goArgs []string,
	finished <-chan struct{},
	interrupt func(),
	cmd *exec.Cmd,
) {
	teaOpts := []tea.ProgramOption{
		tea.WithOutput(os.Stderr),
		tea.WithoutSignalHandler(),
	}
	if term.IsTerminal(os.Stdin.Fd()) {
		teaOpts = append(teaOpts, tea.WithInput(os.Stdin))
	} else {
		teaOpts = append(teaOpts, tea.WithInput(nil))
	}

	prog := tea.NewProgram(newModel(st, opts, eta, goArgs, interrupt), teaOpts...)

	// Raw mode swallows ctrl+c (bubbletea reports it as a key instead), but a
	// signal sent from outside still has to reach the go command.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer close(sigs)
	defer signal.Stop(sigs)

	go func() {
		for sig := range sigs {
			_ = cmd.Process.Signal(sig)
		}
	}()

	go func() {
		<-finished
		prog.Send(finishMsg{})
	}()

	if _, err := prog.Run(); err != nil {
		// The TUI could not start: fall back to waiting quietly.
		<-finished
	}
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "shiny-goggles: %v\n", err)

	return 1
}

func parseArgs(argv []string) (*options, []string, error) {
	opts := &options{mode: ModeSimple}

	var (
		simple, verbose bool
		ignore          repeatedFlag
	)

	fs := flag.NewFlagSet("shiny-goggles", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var monitor bool

	fs.BoolVar(&simple, "simple", false, "")
	fs.BoolVar(&verbose, "verbose", false, "")
	fs.BoolVar(&monitor, "monitor", false, "")
	fs.StringVar(&opts.pprof, "pprof", "", "")
	fs.BoolVar(&opts.goroutines, "goroutines", false, "")
	fs.BoolVar(&opts.clear, "clear", false, "")
	fs.Var(&ignore, "ignore", "")
	fs.IntVar(&opts.top, "top", defaultSlowRows, "")
	fs.IntVar(&opts.recent, "recent", defaultRecentRows, "")
	fs.IntVar(&opts.blocking, "blocking", defaultBlockingRows, "")
	fs.BoolVar(&opts.hideStd, "hide-std", false, "")
	fs.BoolVar(&opts.plain, "plain", false, "")
	fs.StringVar(&opts.trace, "trace", "", "")
	fs.StringVar(&opts.goTrace, "go-trace", "", "")
	fs.StringVar(&opts.actiongraph, "actiongraph", "", "")
	fs.BoolVar(&opts.noHistory, "no-history", false, "")

	if err := fs.Parse(argv); err != nil {
		return nil, nil, err
	}

	switch {
	case monitor:
		opts.mode = ModeMonitor
	case verbose:
		opts.mode = ModeVerbose
	case simple:
		opts.mode = ModeSimple
	}

	opts.ignore = parseIgnore(ignore)

	// Erasing the screen only makes sense for the one that takes it over.
	if opts.clear && opts.mode == ModeSimple {
		opts.mode = ModeVerbose
	}

	// The chart height is reserved before the first package arrives, so the
	// requested size is also its hard limit.
	opts.top = clamp(opts.top, minSlowRows, maxSlowRows)
	opts.recent = clamp(opts.recent, 0, maxRecentRows)
	opts.blocking = clamp(opts.blocking, 0, maxBlockingRows)

	return opts, fs.Args(), nil
}

// normalizeGoCommand lets the user type either the full command or just the go
// subcommand: `shiny-goggles build ./...` == `shiny-goggles go build ./...`.
// Anything that is neither is refused rather than run: silently executing a
// command with no screen attached is the most confusing thing this could do.
func normalizeGoCommand(rest []string) ([]string, error) {
	if len(rest) == 0 {
		return nil, errors.New("nothing to run")
	}

	if isGoCommand(rest[0]) {
		return rest, nil
	}

	if goSubcommands[rest[0]] {
		return append([]string{"go"}, rest...), nil
	}

	return nil, fmt.Errorf("%q is not the go command; shiny-goggles wraps go builds — "+
		"try `shiny-goggles build ./...`, or give a path: `shiny-goggles /opt/go1.25/bin/go build ./...`",
		rest[0])
}

// injectToolexec rewrites `go build ./...` into
// `go build -toolexec="<self> _toolexec" ./...`, plus the optional trace flags.
// It reports false when the command cannot be instrumented, in which case it is
// still returned so the caller can run it untouched.
func injectToolexec(args []string, self string, opts *options) (cmd []string, sub string, ok bool) {
	if len(args) < 2 || !isGoCommand(args[0]) || !toolexecSubcommands[args[1]] {
		if len(args) >= 2 {
			fmt.Fprintf(os.Stderr, "shiny-goggles: nothing to profile in `go %s`, running it as is\n", args[1])
		}

		return args, "", false
	}

	sub = args[1]

	for _, a := range args[2:] {
		if strings.HasPrefix(a, "-toolexec") {
			fmt.Fprintln(os.Stderr, "shiny-goggles: command already sets -toolexec, running unprofiled")

			return args, sub, false
		}
	}

	injected := []string{"-toolexec=" + quoteForToolexec(self) + " " + toolexecMarker}

	if opts.goTrace != "" {
		injected = append(injected, "-debug-trace="+opts.goTrace)
	}
	if opts.actiongraph != "" {
		injected = append(injected, "-debug-actiongraph="+opts.actiongraph)
	}

	cmd = make([]string, 0, len(args)+len(injected))
	cmd = append(cmd, args[:2]...)
	cmd = append(cmd, injected...)
	cmd = append(cmd, args[2:]...)

	return cmd, sub, true
}

// quoteForToolexec quotes a path for the go command, which splits the -toolexec
// value on spaces while honouring double quotes.
func quoteForToolexec(s string) string {
	if !strings.ContainsAny(s, " \t") {
		return s
	}

	if !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}

	return s
}

func execPassthrough(args []string) int {
	if len(args) == 0 {
		return 2
	}

	//nolint:gosec // the command line is what the user asked us to run
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	if err := cmd.Run(); err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}

		return fail(err)
	}

	return 0
}

// repeatedFlag collects a flag that may be given more than once.
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(v string) error {
	*f = append(*f, v)

	return nil
}

func terminalWidth() int {
	w, _, err := term.GetSize(os.Stderr.Fd())
	if err != nil || w <= 0 {
		return 80 // the safe assumption when nobody will tell us
	}

	return w
}
