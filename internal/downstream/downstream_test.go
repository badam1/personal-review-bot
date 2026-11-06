package downstream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInterpret(t *testing.T) {
	cases := []struct {
		name string
		in   State
		want Signal
	}{
		{"deleted outranks all", State{Exists: false, ThumbsUp: 9, Resolved: true}, Removed},
		{"thumbs down outranks up", State{Exists: true, ThumbsUp: 3, ThumbsDown: 1}, Disputed},
		{"thumbs up", State{Exists: true, ThumbsUp: 1}, Endorsed},
		{"resolved", State{Exists: true, Resolved: true}, Endorsed},
		{"nothing happened", State{Exists: true, Replies: 2}, Quiet},
	}
	for _, c := range cases {
		if got := Interpret(c.in); got != c.want {
			t.Errorf("%s: Interpret=%s want %s", c.name, got, c.want)
		}
	}
}

func TestMatch(t *testing.T) {
	threads := []Thread{
		{Path: "a.go", Line: 10, State: State{Exists: true, ThumbsUp: 1}},
		{Path: "b.go", Line: 5, State: State{Exists: true}},
	}
	if tr, ok := Match("a.go", 10, threads); !ok || tr.State.ThumbsUp != 1 {
		t.Errorf("exact match failed: %+v ok=%v", tr, ok)
	}
	if _, ok := Match("a.go", 11, threads); ok {
		t.Error("wrong line should not match")
	}
	if _, ok := Match("a.go", 0, threads); ok {
		t.Error("a finding with no line (design note) should never match")
	}
}

type fakeChecker struct {
	byPR map[int][]Thread
	err  map[int]error
	runs int
}

func (f *fakeChecker) Threads(_ context.Context, _ string, number int) ([]Thread, error) {
	f.runs++
	if e, ok := f.err[number]; ok {
		return nil, e
	}
	return f.byPR[number], nil
}

func TestPoll(t *testing.T) {
	Now = func() time.Time { return time.Unix(0, 0).UTC() } // deterministic
	defer func() { Now = func() time.Time { return time.Now().UTC() } }()

	chk := &fakeChecker{
		byPR: map[int][]Thread{
			1: {{Path: "a.go", Line: 10, State: State{Exists: true, ThumbsUp: 1}}},
			2: {{Path: "c.go", Line: 3, State: State{Exists: false}}},
		},
		err: map[int]error{3: errors.New("rate limited")},
	}
	posted := []Posted{
		{ID: 100, Repo: "o/r", Number: 1, Path: "a.go", Line: 10}, // endorsed
		{ID: 101, Repo: "o/r", Number: 1, Path: "z.go", Line: 99}, // no thread -> unknown
		{ID: 102, Repo: "o/r", Number: 2, Path: "c.go", Line: 3},  // removed
		{ID: 103, Repo: "o/r", Number: 3, Path: "x.go", Line: 1},  // PR errored -> skipped entirely
	}
	got := map[int64]Signal{}
	for _, r := range Poll(context.Background(), posted, chk, nil) {
		got[r.ID] = r.Signal
	}
	want := map[int64]Signal{100: Endorsed, 101: Unknown, 102: Removed}
	if len(got) != len(want) {
		t.Fatalf("got %d results %v, want %d (PR 3 should be skipped)", len(got), got, len(want))
	}
	for id, sig := range want {
		if got[id] != sig {
			t.Errorf("finding %d: %s, want %s", id, got[id], sig)
		}
	}
	if chk.runs != 3 { // one fetch per PR (1,2,3), findings grouped — not one per finding
		t.Errorf("Threads called %d times, want 3 (one per PR)", chk.runs)
	}
}
