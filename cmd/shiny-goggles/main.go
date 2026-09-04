// Command shiny-goggles shows what the go command is actually doing.
//
// It attaches itself to any go build through -toolexec, so it needs nothing
// from the project it profiles:
//
//	shiny-goggles build ./...            # spinner, honest progress bar, current packages
//	shiny-goggles -verbose build ./...   # slowest packages, critical path, timeline
//	shiny-goggles -monitor run . serve   # build, then watch the program run
package main

import (
	"os"

	"github.com/fortael/shiny-goggles/internal/profiler"
)

func main() {
	os.Exit(profiler.Main())
}
