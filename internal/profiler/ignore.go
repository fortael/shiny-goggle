package profiler

import (
	"os"
	"strings"
)

// ignoreEnv sets the patterns without putting them on the command line, which
// is what you want when the command line itself is on screen — a recording, a
// screenshot, a shared terminal.
const ignoreEnv = "SHINY_GOGGLES_IGNORE"

// ignoreList hides packages from the lists, the charts, the timeline and the
// trace. It deliberately does not hide them from the totals: they were built,
// they cost time, and a progress bar that pretended otherwise would be lying.
type ignoreList []string

// parseIgnore collects patterns from the flag and from the environment. Both
// apply — the flag adds to the environment rather than replacing it, so a
// project-wide setting can be extended for one run.
func parseIgnore(flags []string) ignoreList {
	var list ignoreList

	for _, source := range append([]string{os.Getenv(ignoreEnv)}, flags...) {
		for _, pattern := range strings.Split(source, ",") {
			if pattern = strings.TrimSpace(pattern); pattern != "" {
				list = append(list, pattern)
			}
		}
	}

	return list
}

func (l ignoreList) match(pkg string) bool {
	if pkg == "" {
		return false
	}

	for _, pattern := range l {
		if globMatch(pattern, pkg) {
			return true
		}
	}

	return false
}

// globMatch is a deliberately small glob: `*` stands for any run of characters,
// slashes included, so "example.com/org/*" covers a whole tree the way one would
// expect. A pattern without a wildcard names a package or the tree beneath it.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")

	if len(parts) == 1 {
		return s == pattern || strings.HasPrefix(s, pattern+"/")
	}

	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]

	for _, middle := range parts[1 : len(parts)-1] {
		i := strings.Index(s, middle)
		if i < 0 {
			return false
		}
		s = s[i+len(middle):]
	}

	return strings.HasSuffix(s, parts[len(parts)-1])
}
