package profiler

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runToolexec is the hot path: it is executed once per compiled package (plus
// once per asm/cgo/link action), so it must stay allocation-light and must
// never fail the build on its own.
//
// args[0] is the real tool binary chosen by the go command, args[1:] its
// arguments. Stdin/stdout/stderr are inherited untouched — compiler diagnostics
// keep flowing to the go command, which the driver already captures.
func runToolexec(args []string) int {
	// The go command probes tool identities with -V=full to build cache keys.
	// We pass those through verbatim and stay out of the hash, so builds made
	// with and without the profiler share the same build cache.
	for _, a := range args[1:] {
		if a == "-V=full" || a == "-V" {
			return execTool(args)
		}
	}

	client := dialEvents()
	tool := toolName(args[0])
	pkg := os.Getenv("TOOLEXEC_IMPORTPATH")

	client.send(wireEvent{
		Kind: kindStart,
		Tool: tool,
		Pkg:  pkg,
		Std:  isStdBuild(args[1:]),
	})

	// Ask the compiler to report where its own time went. The flag is added
	// here rather than through -gcflags on purpose: the go command never sees
	// it, so the build cache key — and therefore the object file — is the same
	// with and without the profiler.
	var readPhases func() compilePhases

	if client != nil {
		if flag, read := benchFileFor(tool); read != nil {
			// Flags have to precede the source file list the tool ends with.
			args = append([]string{args[0], flag}, args[1:]...)
			readPhases = read
		}
	}

	start := time.Now()
	code := execTool(args)
	elapsed := time.Since(start)

	var phases compilePhases
	if readPhases != nil {
		phases = readPhases() // also removes the temporary file
	}

	client.send(endWireEvent(tool, pkg, elapsed, code, phases))
	client.close()

	return code
}

func endWireEvent(tool, pkg string, elapsed time.Duration, code int, p compilePhases) wireEvent {
	ev := wireEvent{
		Kind: kindEnd,
		Tool: tool,
		Pkg:  pkg,
		Dur:  elapsed.Seconds(),
		Fail: code != 0,
	}

	if !p.empty() {
		ev.Fe = p.frontend.Seconds()
		ev.Be = p.backend.Seconds()
		ev.Lines = p.lines
		ev.Funcs = p.funcs
	}

	return ev
}

func execTool(args []string) int {
	//nolint:gosec // the command and its arguments come from the go toolchain
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}

		return 1
	}

	return 0
}

func toolName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".exe")
}

// isStdBuild reports whether the go command marked this action as part of the
// standard library. cmd/go passes -std to the compiler for std packages, which
// is far more reliable than guessing from the import path (a module named
// "fnd-app" has no dot in its first path element either).
func isStdBuild(args []string) bool {
	for _, a := range args {
		if a == "-std" {
			return true
		}
	}

	return false
}
