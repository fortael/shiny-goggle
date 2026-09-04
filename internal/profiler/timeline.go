package profiler

import (
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// timeSlice is one action placed on the timeline grid: [from, to) columns of a
// lane, the way a trace viewer draws a slice on a track.
type timeSlice struct {
	from int
	to   int
	act  *action
	live bool
}

// timelineRows renders the build the way https://ui.perfetto.dev shows a trace:
// time runs left to right, every action is a rectangle whose width is its
// duration, and rectangles are packed into lanes so parallel work stacks up.
// It always returns exactly `lanes` rows, so the panel never changes height.
func timelineRows(s *styles, sn snap, width, lanes int) []string {
	rows := make([]string, lanes)
	for i := range rows {
		rows[i] = strings.Repeat(" ", max(width, 0))
	}

	if width < 8 || lanes < 1 {
		return rows
	}

	span := sn.elapsed
	if span < time.Millisecond {
		span = time.Millisecond
	}

	column := func(t time.Time) int {
		offset := t.Sub(sn.startedAt)
		if offset < 0 {
			offset = 0
		}

		return clamp(int(float64(offset)/float64(span)*float64(width)), 0, width)
	}

	packed := packLanes(sn, lanes, column, width)

	for i, segs := range packed {
		var b strings.Builder
		cur := 0

		for _, sg := range segs {
			if sg.from > cur {
				b.WriteString(strings.Repeat(" ", sg.from-cur))
			}
			b.WriteString(s.sliceStyle(sg).Render(sliceLabel(sg)))
			cur = sg.to
		}

		if cur < width {
			b.WriteString(strings.Repeat(" ", width-cur))
		}
		rows[i] = b.String()
	}

	return rows
}

// interval is one action on the time axis, finished or still in flight.
type interval struct {
	start time.Time
	end   time.Time
	act   *action
	live  bool
}

// buildIntervals collects every action of the build, oldest first. The sort is
// stable so actions that started in the same instant keep their relative order
// between frames, otherwise the graph flickers as lanes swap.
func buildIntervals(sn snap) []interval {
	items := make([]interval, 0, len(sn.timeline)+len(sn.active))
	for _, a := range sn.timeline {
		items = append(items, interval{start: a.start, end: a.start.Add(a.dur), act: a})
	}

	for i := range sn.active {
		items = append(items, interval{start: sn.active[i].start, end: sn.now, act: &sn.active[i], live: true})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].start.Before(items[j].start) })

	return items
}

// assignLanes packs intervals into lanes: every action goes to the first lane
// that is free when it starts, which is the same greedy stacking a trace viewer
// uses. maxLanes <= 0 means as many lanes as the build actually needed; with a
// limit, the overflow folds into the lane that frees up first — a collapsed
// track rather than a dropped one.
func assignLanes(items []interval, maxLanes int) []int {
	lanes := make([]int, len(items))

	var freeAt []time.Time

	if maxLanes > 0 {
		freeAt = make([]time.Time, maxLanes)
	}

	for i, it := range items {
		lane := -1

		for l := range freeAt {
			if !freeAt[l].After(it.start) {
				lane = l

				break
			}
		}

		switch {
		case lane >= 0:
		case maxLanes <= 0:
			freeAt = append(freeAt, time.Time{})
			lane = len(freeAt) - 1
		default:
			lane = 0
			for l := range freeAt {
				if freeAt[l].Before(freeAt[lane]) {
					lane = l
				}
			}
		}

		freeAt[lane] = it.end
		lanes[i] = lane
	}

	return lanes
}

func packLanes(sn snap, lanes int, column func(time.Time) int, width int) [][]timeSlice {
	items := buildIntervals(sn)
	laneOf := assignLanes(items, lanes)

	packed := make([][]timeSlice, lanes)
	painted := make([]int, lanes)

	for i := range painted {
		painted[i] = -1
	}

	for i, it := range items {
		lane := laneOf[i]

		from, to := column(it.start), column(it.end)
		if to <= from {
			to = from + 1
		}
		to = min(to, width)
		from = max(from, painted[lane]+1)

		if from >= to {
			continue // shorter than the column it would land in
		}

		packed[lane] = append(packed[lane], timeSlice{from: from, to: to, act: it.act, live: it.live})
		painted[lane] = to - 1
	}

	return packed
}

// sliceLabel fills a slice with its package name when the rectangle is wide
// enough to read, and with plain colour when it is not.
func sliceLabel(sg timeSlice) string {
	width := sg.to - sg.from
	if width < 5 {
		return strings.Repeat(" ", width)
	}

	name := pkgLeaf(sg.act.pkg)
	if sg.act.tool != "compile" {
		name = sg.act.tool
	}

	runes := []rune(name)
	if len(runes) > width-2 {
		runes = runes[:width-2]
	}
	name = string(runes)

	return " " + name + strings.Repeat(" ", width-1-lipgloss.Width(name))
}

// pkgLeaf is the part of an import path a human uses to name the package.
func pkgLeaf(pkg string) string {
	if pkg == "" {
		return "?"
	}

	if i := strings.LastIndex(pkg, "/"); i >= 0 && i < len(pkg)-1 {
		return pkg[i+1:]
	}

	return pkg
}
