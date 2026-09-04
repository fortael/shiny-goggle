package profiler

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
)

// planPackages asks the go command what it would compile, without compiling
// anything: `go build -n` prints the tool invocations it would run and honours
// the build cache, so packages that are already cached are simply absent. That
// makes the number of compile lines an honest denominator for a progress bar.
//
// It is deliberately started next to the real build instead of before it: the
// two disagree only about packages the build finished in the meantime, and
// those are exactly the ones we have already counted ourselves.
func planPackages(ctx context.Context, goArgs []string, report func(map[string]bool)) {
	if len(goArgs) < 2 {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := make([]string, 0, len(goArgs)+1)
	args = append(args, goArgs[:2]...)
	args = append(args, "-n")
	args = append(args, goArgs[2:]...)

	//nolint:gosec // the same command the user asked us to run, with -n added
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = io.Discard
	// -n never runs a tool, so no shim can attach; keep the variable out anyway.
	cmd.Env = append(os.Environ(), sockEnv+"=")

	// The go command prints the commands it would run to stderr.
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return
	}

	if err := cmd.Start(); err != nil {
		return
	}

	pkgs := scanCompileLines(pipe)

	if err := cmd.Wait(); err != nil || len(pkgs) == 0 {
		return
	}

	report(pkgs)
}

// scanCompileLines picks the -p import path out of every compile invocation.
func scanCompileLines(r io.Reader) map[string]bool {
	pkgs := make(map[string]bool, 256)
	// Compile lines carry every source file of a package, so they get long.
	br := bufio.NewReaderSize(r, 1<<20)

	for {
		line, err := br.ReadString('\n')
		if pkg, ok := compileTarget(line); ok {
			pkgs[pkg] = true
		}

		if err != nil {
			return pkgs
		}
	}
}

func compileTarget(line string) (string, bool) {
	tool, rest, ok := strings.Cut(line, " ")
	if !ok || !strings.HasSuffix(strings.TrimSuffix(tool, ".exe"), "/compile") {
		return "", false
	}

	fields := strings.Fields(rest)
	for i, f := range fields {
		if f == "-p" && i+1 < len(fields) {
			return fields[i+1], true
		}
	}

	return "", false
}
