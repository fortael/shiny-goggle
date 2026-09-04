package profiler

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// tracePid is arbitrary; a trace only needs the ids to be consistent.
const tracePid = 1

// traceEvent is one entry of the Chrome Trace Event format, which
// https://ui.perfetto.dev reads directly.
type traceEvent struct {
	Name string         `json:"name"`
	Cat  string         `json:"cat,omitempty"`
	Ph   string         `json:"ph"`
	Ts   int64          `json:"ts"` // microseconds from the start of the build
	Dur  int64          `json:"dur,omitempty"`
	Pid  int            `json:"pid"`
	Tid  int            `json:"tid"`
	Args map[string]any `json:"args,omitempty"`
}

type traceFile struct {
	DisplayTimeUnit string       `json:"displayTimeUnit"`
	Events          []traceEvent `json:"traceEvents"`
}

// writeTrace saves the build as a trace for https://ui.perfetto.dev.
//
// The go command's own -debug-trace is not used for this: it emits a flow event
// per action dependency, thousands of which do not land inside a slice, so
// perfetto reports them as FLOW_NO_ENCLOSING_SLICE errors. And with `go run` it
// keeps recording for as long as the program runs, which buries the build in an
// otherwise empty timeline. This one contains the build and nothing else: one
// track per parallel worker, one complete slice per tool invocation.
func writeTrace(path, title string, sn snap) error {
	items := buildIntervals(sn)
	lanes := assignLanes(items, 0)

	events := make([]traceEvent, 0, len(items)+len(sn.samples)+8)
	events = append(events, traceEvent{
		Name: "process_name", Ph: "M", Pid: tracePid, Tid: 0,
		Args: map[string]any{"name": "go build: " + title},
	})

	// A slice spanning the whole build, so the trace opens on something.
	span := map[string]any{
		"packages":       sn.totalPackages(),
		"stdlib":         sn.std,
		"cpu_ms":         sn.cpu.Milliseconds(),
		"parallelism":    fmt.Sprintf("%.2f", sn.parallelism()),
		"peak":           sn.peakPar,
		"serial_ms":      sn.serial.Milliseconds(),
		"idle_ms":        sn.idle.Milliseconds(),
		"serial_percent": fmt.Sprintf("%.0f", sn.serialShare()*100),
	}
	if len(sn.timeline) >= keptTimeline {
		span["truncated_after"] = keptTimeline
	}

	events = append(events, traceEvent{
		Name: title, Cat: "build", Ph: "X", Pid: tracePid, Tid: 0,
		Ts: 0, Dur: sn.elapsed.Microseconds(), Args: span,
	})

	laneCount := 0
	for i, it := range items {
		tid := lanes[i] + 1
		laneCount = max(laneCount, tid)

		a := it.act
		events = append(events, traceEvent{
			Name: sliceName(a),
			Cat:  a.tool,
			Ph:   "X",
			Pid:  tracePid,
			Tid:  tid,
			Ts:   max(it.start.Sub(sn.startedAt).Microseconds(), 0),
			Dur:  max(it.end.Sub(it.start).Microseconds(), 1),
			Args: sliceArgs(a, it.live),
		})
	}

	for lane := 1; lane <= laneCount; lane++ {
		events = append(events, traceEvent{
			Name: "thread_name", Ph: "M", Pid: tracePid, Tid: lane,
			Args: map[string]any{"name": fmt.Sprintf("worker %d", lane)},
		})
		events = append(events, traceEvent{
			Name: "thread_sort_index", Ph: "M", Pid: tracePid, Tid: lane,
			Args: map[string]any{"sort_index": lane},
		})
	}

	events = append(events, parallelismCounters(sn)...)

	data, err := json.Marshal(traceFile{DisplayTimeUnit: "ms", Events: events})
	if err != nil {
		return fmt.Errorf("encode trace: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write trace: %w", err)
	}

	return nil
}

func sliceName(a *action) string {
	name := a.pkg
	if name == "" {
		name = "(no package)"
	}

	if a.tool != "compile" {
		return a.tool + " " + name
	}

	return name
}

func sliceArgs(a *action, live bool) map[string]any {
	args := map[string]any{
		"package":  a.pkg,
		"tool":     a.tool,
		"stdlib":   a.std,
		"duration": fmtDur(a.dur),
	}

	if a.solo > 0 {
		args["solo"] = fmtDur(a.solo)
		args["blocking"] = a.blocking()
	}
	if a.expect > 0 {
		args["usually"] = fmtDur(a.expect)
	}
	if a.failed {
		args["failed"] = true
	}
	if live {
		args["still_running"] = true
	}

	return args
}

// parallelismCounters turn the sampled concurrency into a counter track, which
// perfetto draws as an area chart above the workers.
func parallelismCounters(sn snap) []traceEvent {
	events := make([]traceEvent, 0, len(sn.samples))
	for i, v := range sn.samples {
		events = append(events, traceEvent{
			Name: "running tools", Ph: "C", Pid: tracePid, Tid: 0,
			Ts:   (time.Duration(i) * uiTick).Microseconds(),
			Args: map[string]any{"tools": v},
		})
	}

	return events
}
