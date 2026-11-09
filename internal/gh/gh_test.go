package gh

import (
	"bytes"
	"testing"
)

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

// The avatar is fetched with a plain client, so the only thing standing between that fetch and an arbitrary
// address is this check: the URL comes from the API, and the guard keeps a changed field or a redirect from
// pointing it somewhere else.
// Only the picture types GitHub stores avatars as are served on: not SVG, which can carry script, nor HTML.
func TestAvatarType(t *testing.T) {
	for header, want := range map[string]bool{"image/png": true, "image/jpeg; charset=binary": true, " IMAGE/WEBP ": true,
		"image/gif": true, "image/svg+xml": false, "text/html": false, "": false, "image/png-ish": false} {
		if _, ok := avatarType(header); ok != want {
			t.Errorf("avatarType(%q) = %v, want %v", header, ok, want)
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
		{"the repo is itself ..", "https://github.com/acme/../pull/1", "", 0, false},
		{"the repo is itself .", "https://github.com/acme/./pull/1", "", 0, false},
		{"not a PR at all", "https://github.com/acme/api/issues/42", "", 0, false},
	}
	for _, c := range cases {
		repo, n, err := ParseURL(c.url)
		if (err == nil) != c.ok || repo != c.repo || n != c.number {
			t.Errorf("%s: got (%q, %d, %v), want (%q, %d, ok=%v)", c.name, repo, n, err, c.repo, c.number, c.ok)
		}
	}
}

func TestAvatarHost(t *testing.T) {
	cases := []struct {
		name, url string
		want      bool
	}{
		{"an avatar", "https://avatars.githubusercontent.com/u/1?v=4", true},
		{"githubusercontent itself", "https://githubusercontent.com/u/1", false},
		{"raw files anyone can push", "https://raw.githubusercontent.com/a/b", false},
		{"uploaded images", "https://user-images.githubusercontent.com/1/x.svg", false},
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

// A picture over the cap is refused, not cut short. io.LimitReader stops at its limit without an error, so a
// truncated picture would otherwise be cached and served as a whole one.
func TestReadAvatar(t *testing.T) {
	for _, n := range []int{0, 1024, avatarMax - 1, avatarMax} {
		b, err := readAvatar(bytes.NewReader(make([]byte, n)))
		if err != nil || len(b) != n {
			t.Errorf("readAvatar(%d bytes) = %d bytes, %v; want %d bytes, no error", n, len(b), err, n)
		}
	}
	for _, n := range []int{avatarMax + 1, avatarMax * 2} {
		if b, err := readAvatar(bytes.NewReader(make([]byte, n))); err == nil {
			t.Errorf("readAvatar(%d bytes) returned %d bytes, want an error", n, len(b))
		}
	}
}

// GitHub's REST API calls a merged PR closed; the page tells them apart, and an open draft from an open PR.
func TestPRState(t *testing.T) {
	for _, c := range []struct {
		state         string
		merged, draft bool
		want          string
	}{
		{"open", false, false, "open"}, {"open", false, true, "draft"}, {"closed", true, false, "merged"},
		{"closed", false, false, "closed"}, {"closed", false, true, "closed"},
	} {
		if got := prState(c.state, c.merged, c.draft); got != c.want {
			t.Errorf("prState(%q, merged %t, draft %t) = %q, want %q", c.state, c.merged, c.draft, got, c.want)
		}
	}
}
