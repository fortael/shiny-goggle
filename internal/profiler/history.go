package profiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxHistoryPackages caps the per-package timing cache so the file cannot grow
// without bound in a workspace with many modules.
const (
	maxHistoryPackages = 4000
	keptDurations      = 7
	packageEMAAlpha    = 0.4
)

// history remembers how long previous builds took in this directory. It gives
// the TUI two things it cannot know on its own: an ETA for the whole build and
// a "usually takes ~Ns" hint for every package that is currently compiling.
type history struct {
	path     string
	Commands map[string][]float64 `json:"commands"` // go command -> recent wall clock seconds
	Packages map[string]float64   `json:"packages"` // import path -> smoothed compile seconds
}

func historyPath(dir string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(dir))
	root := filepath.Join(cache, "shiny-goggles")

	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}

	return filepath.Join(root, hex.EncodeToString(sum[:8])+".json"), nil
}

func loadHistory(dir string) *history {
	h := &history{
		Commands: map[string][]float64{},
		Packages: map[string]float64{},
	}

	path, err := historyPath(dir)
	if err != nil {
		return h
	}
	h.path = path

	data, err := os.ReadFile(path) //nolint:gosec // path is derived from the user cache dir
	if err != nil {
		return h
	}

	var stored history
	if err := json.Unmarshal(data, &stored); err != nil {
		return h
	}

	if stored.Commands != nil {
		h.Commands = stored.Commands
	}
	if stored.Packages != nil {
		h.Packages = stored.Packages
	}

	return h
}

// expectations converts the stored package timings into durations.
func (h *history) expectations() map[string]time.Duration {
	out := make(map[string]time.Duration, len(h.Packages))
	for pkg, sec := range h.Packages {
		out[pkg] = time.Duration(sec * float64(time.Second))
	}

	return out
}

// eta predicts the wall clock duration of the given command, or 0 when this
// command has not been recorded yet. The median of the recent runs is used so a
// single cold build does not poison the estimate.
func (h *history) eta(key string) time.Duration {
	runs := h.Commands[key]
	if len(runs) == 0 {
		return 0
	}

	sorted := append([]float64(nil), runs...)
	sort.Float64s(sorted)

	return time.Duration(sorted[len(sorted)/2] * float64(time.Second))
}

// record stores the outcome of a successful build.
func (h *history) record(key string, elapsed time.Duration, sn snap) {
	if h.path == "" {
		return
	}

	runs := append(h.Commands[key], elapsed.Seconds())
	if len(runs) > keptDurations {
		runs = runs[len(runs)-keptDurations:]
	}
	h.Commands[key] = runs

	for _, a := range sn.slow {
		h.mergePackage(a)
	}
	for _, a := range sn.recent {
		h.mergePackage(a)
	}

	h.prune()

	data, err := json.Marshal(h)
	if err != nil {
		return
	}

	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, h.path)
}

func (h *history) mergePackage(a *action) {
	if a.failed || a.tool != "compile" || a.pkg == "" {
		return
	}

	sec := a.dur.Seconds()
	if prev, ok := h.Packages[a.pkg]; ok {
		sec = prev*(1-packageEMAAlpha) + sec*packageEMAAlpha
	}

	h.Packages[a.pkg] = sec
}

// prune drops the cheapest packages once the cache grows too large; only the
// slow ones are worth remembering, they are the ones we annotate in the UI.
func (h *history) prune() {
	if len(h.Packages) <= maxHistoryPackages {
		return
	}

	type entry struct {
		pkg string
		sec float64
	}

	all := make([]entry, 0, len(h.Packages))
	for pkg, sec := range h.Packages {
		all = append(all, entry{pkg, sec})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sec > all[j].sec })

	kept := make(map[string]float64, maxHistoryPackages)
	for _, e := range all[:maxHistoryPackages] {
		kept[e.pkg] = e.sec
	}
	h.Packages = kept
}

// commandKey identifies a go invocation for ETA purposes. Flags injected by the
// profiler itself are not part of it.
func commandKey(args []string) string { return strings.Join(args, " ") }
