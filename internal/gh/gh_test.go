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

// The avatar is fetched with a plain client, so the only thing standing between that fetch and an arbitrary
// address is this check: the URL comes from the API, and the guard keeps a changed field or a redirect from
// pointing it somewhere else.
func TestAvatarHost(t *testing.T) {
	cases := []struct {
		name, url string
		want      bool
	}{
		{"an avatar", "https://avatars.githubusercontent.com/u/1?v=4", true},
		{"githubusercontent itself", "https://githubusercontent.com/u/1", true},
		{"another githubusercontent subdomain", "https://raw.githubusercontent.com/a/b", true},
		{"plain http", "http://avatars.githubusercontent.com/u/1", false},
		{"somewhere else", "https://evil.example/u/1", false},
		{"a lookalike suffix", "https://githubusercontent.com.evil.example/u/1", false},
		{"a lookalike prefix", "https://avatars.githubusercontent.com.evil.example/u/1", false},
		{"userinfo pointing elsewhere", "https://avatars.githubusercontent.com@evil.example/u/1", false},
		{"empty", "", false},
		{"not a URL", "://", false},
	}
	for _, c := range cases {
		if got := avatarHost(c.url); got != c.want {
			t.Errorf("%s: avatarHost(%q) = %v, want %v", c.name, c.url, got, c.want)
		}
	}
}
