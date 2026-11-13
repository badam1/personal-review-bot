package metrics

import (
	"math"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name, authored, final string
		want                  EditKind
	}{
		{"never captured", "", "anything", Unknown},
		{"whitespace only", "A nil map.", "  A   nil  map.\n", Verbatim},
		{"identical", "A nil map panics on write.", "A nil map panics on write.", Verbatim},
		{"reworded", "A nil map panics on write.", "This map is nil and will panic.", Edited},
		{"trimmed addition", "Guard the nil map.", "Guard the nil map before writing.", Edited},
	}
	for _, c := range cases {
		if got := Classify(c.authored, c.final); got != c.want {
			t.Errorf("%s: Classify(%q,%q)=%s want %s", c.name, c.authored, c.final, got, c.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if got := Similarity("", "x"); got != 0 {
		t.Errorf("empty authored: got %v want 0", got)
	}
	if got := Similarity("same text", "same   text"); got != 1 {
		t.Errorf("whitespace-only diff: got %v want 1", got)
	}
	full := Similarity("abcdef", "uvwxyz")
	if full > 0.01 {
		t.Errorf("full rewrite: got %v want ~0", full)
	}
	// A one-character tweak in a longer string should stay close to 1.
	near := Similarity("the quick brown fox", "the quick brown box")
	if near < 0.9 || near > 1 {
		t.Errorf("one-char edit: got %v want ~0.95", near)
	}
	// Monotonic sanity: a small edit is more similar than a big one.
	if Similarity("hello world", "hello worlds") <= Similarity("hello world", "goodbye") {
		t.Error("a small edit should score higher than a large one")
	}
	if math.IsNaN(near) {
		t.Error("similarity should never be NaN")
	}
}
