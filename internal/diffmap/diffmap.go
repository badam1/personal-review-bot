// Package diffmap works out which lines a review comment can be attached to: GitHub only accepts inline comments
// on lines that appear in the diff (added or context lines on the new side).
package diffmap

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/badam1/personal-review-bot/internal/gh"
)

type Map map[string]map[int]bool

var hunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func FromFiles(files []gh.File) Map {
	m := Map{}
	for _, f := range files {
		m[f.Path], _ = parsePatch(f.Patch)
	}
	return m
}

// Added returns, per file, the new-side lines the PR adds ("+" lines), as opposed to unchanged context.
func Added(files []gh.File) Map {
	m := Map{}
	for _, f := range files {
		_, m[f.Path] = parsePatch(f.Patch)
	}
	return m
}

func parsePatch(patch string) (inDiff, added map[int]bool) {
	inDiff, added = map[int]bool{}, map[int]bool{}
	n := 0
	for _, l := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := hunk.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
			// removed line or "\ No newline at end of file": nothing on the new side
		default: // added ("+") or context (" ") line
			if n > 0 {
				inDiff[n] = true
				added[n] = strings.HasPrefix(l, "+")
				n++
			}
		}
	}
	return inDiff, added
}

// Place returns the line to attach a comment to: the wanted line if it is in the diff, otherwise the nearest diff
// line within 3 lines. ok is false when there is none (the comment then goes into the review body).
func (m Map) Place(path string, want int) (line int, ok bool) {
	lines := m[path]
	if lines[want] {
		return want, true
	}
	for d := 1; d <= 3; d++ {
		if lines[want+d] {
			return want + d, true
		}
		if lines[want-d] {
			return want - d, true
		}
	}
	return want, false
}
