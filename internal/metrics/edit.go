// Package metrics turns the raw signals the store captures into the measures the dashboard reads. It is pure: no
// database, no network, no clock. Each function takes values and returns a value, so a measure can be tested on its
// own and changed without a migration, and nothing here depends on the rest of the program.
package metrics

import (
	"strings"
	"unicode/utf8"
)

// EditKind says how a posted finding's text compares with what the bot proposed.
type EditKind int

const (
	Unknown  EditKind = iota // the bot's proposal was never captured (a finding from before this was recorded)
	Verbatim                 // posted word for word, give or take whitespace: the bot's words, as yours
	Edited                   // reworded before it went out
)

func (k EditKind) String() string {
	switch k {
	case Verbatim:
		return "verbatim"
	case Edited:
		return "edited"
	default:
		return "unknown"
	}
}

// Classify compares what the bot proposed (authored) with what was posted (final). An empty authored means the
// proposal was never recorded, so the answer is Unknown rather than a guess that it was kept word for word.
func Classify(authored, final string) EditKind {
	if strings.TrimSpace(authored) == "" {
		return Unknown
	}
	if normalize(authored) == normalize(final) {
		return Verbatim
	}
	return Edited
}

// Similarity is how much of the bot's proposal survived into what was posted: 1 is untouched, 0 is rewritten whole.
// It is 1 minus the edit distance over the longer length, after whitespace is folded. An empty authored returns 0,
// since nothing is known to have survived.
func Similarity(authored, final string) float64 {
	if strings.TrimSpace(authored) == "" {
		return 0
	}
	a, b := normalize(authored), normalize(final)
	if a == b {
		return 1
	}
	longer := utf8.RuneCountInString(a)
	if n := utf8.RuneCountInString(b); n > longer {
		longer = n
	}
	if longer == 0 {
		return 1
	}
	return 1 - float64(levenshtein([]rune(a), []rune(b)))/float64(longer)
}

// normalize folds the differences that are not edits: surrounding space, and any run of whitespace down to one space.
func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// levenshtein is the edit distance between two rune slices, in O(len(a)·len(b)) time and O(len(b)) space.
func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev = cur
	}
	return prev[len(b)]
}
