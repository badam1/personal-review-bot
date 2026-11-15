package downstream

import "testing"

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
