package diffmap

import (
	"testing"

	"github.com/badam1/personal-review-bot/internal/gh"
)

const patch = `@@ -10,4 +10,5 @@ func a() {
 ctx := 1
-old := 2
+new := 2
+added := 3
 tail := 4
@@ -40,2 +41,2 @@ func b() {
 keep
-gone
+back
\ No newline at end of file`

func TestFromFilesAndPlace(t *testing.T) {
	m := FromFiles([]gh.File{{Path: "a.go", Patch: patch}})
	for _, n := range []int{10, 11, 12, 13, 41, 42} {
		if !m["a.go"][n] {
			t.Errorf("line %d should be commentable", n)
		}
	}
	for _, n := range []int{9, 14, 40, 43} {
		if m["a.go"][n] {
			t.Errorf("line %d should not be commentable", n)
		}
	}
	cases := []struct {
		want, got int
		ok        bool
	}{{11, 11, true}, {15, 13, true}, {30, 30, false}, {44, 42, true}}
	for _, c := range cases {
		got, ok := m.Place("a.go", c.want)
		if got != c.got || ok != c.ok {
			t.Errorf("Place(%d) = %d,%v; want %d,%v", c.want, got, ok, c.got, c.ok)
		}
	}
	if _, ok := m.Place("other.go", 1); ok {
		t.Error("file not in the diff must not be inline")
	}
}

// Added tells the lines a PR adds from the unchanged context around them.
func TestAdded(t *testing.T) {
	patch := "@@ -1,3 +1,4 @@\n a\n-b\n+B\n+C\n d"
	m := Added([]gh.File{{Path: "x.go", Patch: patch}})
	for line, want := range map[int]bool{1: false, 2: true, 3: true, 4: false} {
		if m["x.go"][line] != want {
			t.Errorf("line %d added = %v, want %v", line, m["x.go"][line], want)
		}
	}
}
