package profiler

import (
	"strconv"
	"strings"
	"time"
)

// testStatus is the verdict `go test` printed for one package.
type testStatus int

const (
	testPassed testStatus = iota
	testFailed
	testCached
	testNoTests
)

type testResult struct {
	pkg    string
	status testStatus
	dur    time.Duration
	fails  int // failing test functions seen for this package
}

// parseTestLine turns one line of `go test` output into a package result.
// Only the package level verdicts are recognised; everything else is output the
// user gets to read verbatim once the screen is handed back.
//
//	ok  	fnd-app/internal/api	0.123s
//	ok  	fnd-app/internal/api	(cached)
//	FAIL	fnd-app/internal/api	0.456s
//	?   	fnd-app/cmd	[no test files]
func parseTestLine(line string) (testResult, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return testResult{}, false
	}

	res := testResult{pkg: fields[1]}

	switch fields[0] {
	case "ok":
		res.status = testPassed
	case "FAIL":
		res.status = testFailed
	case "?":
		res.status = testNoTests
	default:
		return testResult{}, false
	}

	// "FAIL" also prefixes the final summary line and build failures; those
	// carry no package path shaped token.
	if strings.ContainsAny(res.pkg, "[]") || res.pkg == "" {
		return testResult{}, false
	}

	if len(fields) > 2 {
		switch {
		case fields[2] == "(cached)":
			res.status = testCached
		case strings.HasSuffix(fields[2], "s"):
			if secs, err := strconv.ParseFloat(strings.TrimSuffix(fields[2], "s"), 64); err == nil {
				res.dur = time.Duration(secs * float64(time.Second))
			}
		}
	}

	return res, true
}

// isTestFailure spots an individual failing test function, so the count in the
// panel is about tests and not only about packages.
func isTestFailure(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "--- FAIL:")
}

// testTally counts package verdicts for the header of the panel.
func testTally(results []testResult) (passed, failed, skipped int) {
	for _, r := range results {
		switch r.status {
		case testPassed, testCached:
			passed++
		case testFailed:
			failed++
		case testNoTests:
			skipped++
		}
	}

	return passed, failed, skipped
}
