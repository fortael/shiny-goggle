package profiler

import (
	"fmt"
	"os"
	"time"
)

// plainHeartbeat is how often a progress line is printed when we are not
// attached to a terminal (CI logs, `docker compose exec -T`, piped output).
const plainHeartbeat = 2 * time.Second

// runPlain keeps sampling the build so the summary is still complete, and emits
// one compact line every couple of seconds instead of taking over the terminal.
func runPlain(st *stats, opts *options, finished <-chan struct{}) {
	ticker := time.NewTicker(uiTick)
	defer ticker.Stop()

	last := time.Now()

	for {
		select {
		case <-finished:
			return

		case <-ticker.C:
			st.sample()

			if time.Since(last) < plainHeartbeat {
				continue
			}
			last = time.Now()

			printPlainProgress(st.snapshot(), opts)
		}
	}
}

func printPlainProgress(sn snap, opts *options) {
	line := fmt.Sprintf("  [%7s] %d pkgs · %d running", fmtDur(sn.elapsed), sn.compiled, len(sn.active))

	if slowest := longestRunning(sn); slowest != nil {
		line += fmt.Sprintf(" · now %s (%s)",
			truncate(slowest.pkg, 60), fmtDur(sn.now.Sub(slowest.start)))
	}

	if top := sn.visibleSlow(!opts.hideStd, 1); len(top) > 0 {
		line += fmt.Sprintf(" · slowest %s (%s)", truncate(top[0].pkg, 60), fmtDur(top[0].dur))
	}

	fmt.Fprintln(os.Stderr, line)
}

func longestRunning(sn snap) *action {
	if len(sn.active) == 0 {
		return nil
	}

	// snap.active is already ordered by start time.
	return &sn.active[0]
}
