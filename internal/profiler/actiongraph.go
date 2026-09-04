package profiler

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// maxBlastRoots caps how many "you changed this" packages are named; past a
// handful it is a cold build and the list stops meaning anything.
const maxBlastRoots = 6

// graphAction is the part of the go command's -debug-actiongraph we use. The
// graph is written when the build finishes and before `go run` starts the
// program, so it is available for every subcommand.
type graphAction struct {
	ID        int    `json:"ID"`
	Mode      string `json:"Mode"`
	Package   string `json:"Package"`
	Deps      []int  `json:"Deps"`
	NeedBuild bool   `json:"NeedBuild"`
	TimeReady string `json:"TimeReady"`
	TimeStart string `json:"TimeStart"`
	TimeDone  string `json:"TimeDone"`
}

// link is one step of the critical path.
type link struct {
	pkg  string
	mode string
	dur  time.Duration
}

// blastRoot is a package that was rebuilt although none of its dependencies
// were — in other words, one you actually changed — and how many other packages
// had to be rebuilt because of it.
type blastRoot struct {
	pkg        string
	downstream int
}

// buildGraph is what the action graph tells us that watching tools cannot: the
// dependency chain that actually set the build's duration, and which packages
// were rebuilt versus served from the cache.
type buildGraph struct {
	actions   int
	built     int
	cached    int
	wall      time.Duration
	critical  time.Duration
	path      []link
	roots     []blastRoot
	queueWait time.Duration
}

// slack is the time the build would save on an unlimited number of cores: what
// is left once the critical path is taken out of the wall clock.
func (g *buildGraph) slack() time.Duration { return max(g.wall-g.critical, 0) }

func (g *buildGraph) criticalShare() float64 {
	if g.wall <= 0 {
		return 0
	}

	return g.critical.Seconds() / g.wall.Seconds()
}

//nolint:gocyclo,funlen // one pass over the graph answering three questions
func loadGraph(path string) (*buildGraph, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path we chose ourselves
	if err != nil {
		return nil, fmt.Errorf("read action graph: %w", err)
	}

	var actions []graphAction
	if err := json.Unmarshal(data, &actions); err != nil {
		return nil, fmt.Errorf("parse action graph: %w", err)
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("action graph is empty")
	}

	byID := make(map[int]*graphAction, len(actions))
	for i := range actions {
		byID[actions[i].ID] = &actions[i]
	}

	g := &buildGraph{actions: len(actions)}

	var first, last time.Time

	for i := range actions {
		a := &actions[i]
		// Only "build" actions can be cached or rebuilt; the rest are the go
		// command's own bookkeeping and would inflate both numbers.
		if a.Mode == "build" && a.Package != "" {
			if a.NeedBuild {
				g.built++
			} else {
				g.cached++
			}
		}

		start, done := parseGraphTime(a.TimeStart), parseGraphTime(a.TimeDone)
		if start.IsZero() {
			continue
		}
		if first.IsZero() || start.Before(first) {
			first = start
		}
		if done.After(last) {
			last = done
		}
		// Ready but no worker free: the build was short of parallelism here.
		if ready := parseGraphTime(a.TimeReady); !ready.IsZero() && start.After(ready) {
			g.queueWait += start.Sub(ready)
		}
	}

	g.wall = last.Sub(first)
	g.critical, g.path = criticalPath(actions, byID)
	g.roots = blastRoots(actions, byID)

	return g, nil
}

// criticalPath walks the dependency graph for the longest chain of durations —
// the sequence of actions that had to happen one after another. Nothing else in
// the build can be blamed for the wall clock.
func criticalPath(actions []graphAction, byID map[int]*graphAction) (time.Duration, []link) {
	type memoized struct {
		total time.Duration
		next  int
	}

	memo := make(map[int]memoized, len(actions))

	var longest func(id int) memoized

	longest = func(id int) memoized {
		if m, ok := memo[id]; ok {
			return m
		}

		a, ok := byID[id]
		if !ok {
			return memoized{next: -1}
		}

		// Guard against a cycle: the go command should never emit one, but a
		// stack overflow would be a terrible way to find out.
		memo[id] = memoized{next: -1}

		best := memoized{next: -1}
		for _, dep := range a.Deps {
			if m := longest(dep); m.total > best.total {
				best = memoized{total: m.total, next: dep}
			}
		}

		result := memoized{total: best.total + actionDuration(a), next: best.next}
		memo[id] = result

		return result
	}

	tail := -1
	for i := range actions {
		if actions[i].TimeDone == "" {
			continue
		}
		if m := longest(actions[i].ID); tail < 0 || m.total > memo[tail].total {
			tail = actions[i].ID
		}
	}

	if tail < 0 {
		return 0, nil
	}

	var path []link
	for id := tail; id >= 0; id = memo[id].next {
		a, ok := byID[id]
		if !ok {
			break
		}
		if d := actionDuration(a); d > 0 {
			path = append(path, link{pkg: a.Package, mode: a.Mode, dur: d})
		}
	}

	return memo[tail].total, path
}

// blastRoots finds the packages whose own sources changed — rebuilt while every
// dependency came from the cache — and counts what had to be rebuilt after them.
func blastRoots(actions []graphAction, byID map[int]*graphAction) []blastRoot {
	dependents := make(map[int][]int, len(actions))
	for i := range actions {
		for _, dep := range actions[i].Deps {
			dependents[dep] = append(dependents[dep], actions[i].ID)
		}
	}

	var roots []blastRoot

	for i := range actions {
		a := &actions[i]
		if !a.NeedBuild || a.Package == "" || a.Mode != "build" {
			continue
		}

		fresh := true
		for _, dep := range a.Deps {
			if d, ok := byID[dep]; ok && d.NeedBuild {
				fresh = false

				break
			}
		}
		if !fresh {
			continue
		}

		roots = append(roots, blastRoot{pkg: a.Package, downstream: reachableBuilt(a.ID, dependents, byID)})
		if len(roots) > maxBlastRoots {
			// A cold build makes everything a root; the list stops being news.
			return nil
		}
	}

	sort.Slice(roots, func(i, j int) bool { return roots[i].downstream > roots[j].downstream })

	return roots
}

// reachableBuilt counts the actions that were rebuilt downstream of one action.
func reachableBuilt(from int, dependents map[int][]int, byID map[int]*graphAction) int {
	seen := map[int]bool{from: true}
	queue := []int{from}
	count := 0

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		for _, next := range dependents[id] {
			if seen[next] {
				continue
			}
			seen[next] = true

			a, ok := byID[next]
			if !ok || !a.NeedBuild {
				continue
			}
			if a.Mode == "build" && a.Package != "" {
				count++
			}

			queue = append(queue, next)
		}
	}

	return count
}

func actionDuration(a *graphAction) time.Duration {
	start, done := parseGraphTime(a.TimeStart), parseGraphTime(a.TimeDone)
	if start.IsZero() || done.Before(start) {
		return 0
	}

	return done.Sub(start)
}

func parseGraphTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}

	return t
}
