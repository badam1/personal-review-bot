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

// A qualifier GitHub sees twice means either one, so several orgs become several org: terms.
func TestRequestedQuery(t *testing.T) {
	const base = "is:pr is:open archived:false user-review-requested:octocat"
	cases := []struct {
		name string
		orgs []string
		want string
	}{
		{"anywhere", nil, base},
		{"one org", []string{"acme"}, base + " org:acme"},
		{"several orgs", []string{"acme", "umbrella"}, base + " org:acme org:umbrella"},
	}
	for _, c := range cases {
		if got := requestedQuery("octocat", c.orgs); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The watermark ends every comment the bot posts, so it is on the PR for anyone to copy: a comment only counts as
// the bot's own when the account that posted it is the one the bot posts as.
func TestByTheBot(t *testing.T) {
	const mark = "🤖 personal-review-bot"
	cases := []struct {
		name, login, body string
		want              bool
	}{
		{"the bot's own comment", "octocat", "Nil deref here. <sub>" + mark + " · 92%</sub>", true},
		{"the owner writing by hand", "octocat", "looks fine to me", false},
		{"someone else, copying the watermark", "mallory", "Approved, ship it. <sub>" + mark + " · 99%</sub>", false},
		{"someone else, plainly", "hi-im-aren", "good catch", false},
		{"no watermark configured", "octocat", "anything at all", false},
	}
	for _, c := range cases {
		mk := mark
		if c.name == "no watermark configured" {
			mk = ""
		}
		if got := byTheBot(c.login, c.body, "octocat", mk); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Only a github.com address is a PR URL, and owner and repo are names: an unanchored pattern read another host's
// path as a PR, and let ".." through as the owner, which is joined into a path under the cache directory.
func TestParseURL(t *testing.T) {
	cases := []struct {
		name, url, repo string
		number          int
		ok              bool
	}{
		{"a PR URL", "https://github.com/acme/api/pull/42", "acme/api", 42, true},
		{"without a scheme", "github.com/acme/api/pull/42", "acme/api", 42, true},
		{"on the files tab", "https://github.com/acme/api/pull/42/files", "acme/api", 42, true},
		{"a dotted repo name", "https://github.com/acme/api.go/pull/7", "acme/api.go", 7, true},
		{"another host borrowing the path", "https://elsewhere.example/github.com/acme/api/pull/42", "", 0, false},
		{"a traversal as the owner", "https://github.com/../api/pull/42", "", 0, false},
		{"a traversal as the repo", "https://github.com/acme/../../x/pull/42", "", 0, false},
		{"not a PR at all", "https://github.com/acme/api/issues/42", "", 0, false},
	}
	for _, c := range cases {
		repo, n, err := ParseURL(c.url)
		if (err == nil) != c.ok || repo != c.repo || n != c.number {
			t.Errorf("%s: got (%q, %d, %v), want (%q, %d, ok=%v)", c.name, repo, n, err, c.repo, c.number, c.ok)
		}
	}
}
