package gh

import "testing"

// The approval text is said once per PR; only my own approvals (dismissed ones too) count.
func TestSaidIn(t *testing.T) {
	review := func(login, state, body string) Review {
		r := Review{State: state, Body: body}
		r.User.Login = login
		return r
	}
	cases := []struct {
		name    string
		reviews []Review
		want    bool
	}{
		{"never approved", nil, false},
		{"approved with it", []Review{review("octocat", "APPROVED", "LGTM :+1:")}, true},
		{"approval dismissed by a push", []Review{review("octocat", "DISMISSED", "LGTM :+1:")}, true},
		{"typed by hand, emoji and lower case", []Review{review("octocat", "APPROVED", "lgtm 👍 nice")}, true},
		{"approved without it", []Review{review("octocat", "APPROVED", "")}, false},
		{"only commented it", []Review{review("octocat", "COMMENTED", "LGTM :+1:")}, false},
		{"someone else said it", []Review{review("hi-im-aren", "APPROVED", "LGTM :+1:")}, false},
	}
	for _, c := range cases {
		if got := saidIn(c.reviews, "octocat", "LGTM :+1:"); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
