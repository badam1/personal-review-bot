package gh

import (
	"bytes"
	"io"
	"net/http"
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

// The avatar is fetched with a plain client, so the only thing standing between that fetch and an arbitrary
// address is this check: the URL comes from the API, and the guard keeps a changed field or a redirect from
// pointing it somewhere else.
func TestAvatarHost(t *testing.T) {
	cases := []struct {
		name, url string
		want      bool
	}{
		{"an avatar", "https://avatars.githubusercontent.com/u/1?v=4", true},
		{"githubusercontent itself", "https://githubusercontent.com/u/1", false},
		{"raw, which serves whatever a user committed", "https://raw.githubusercontent.com/a/b", false},
		{"another githubusercontent subdomain", "https://objects.githubusercontent.com/a/b", false},
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

// An SVG is not drawn as a flat picture: it can carry script, and the bot serves these bytes from its own origin.
// A picture over the cap is refused outright, because a truncated one would be cached and served as a whole one.
func TestPicture(t *testing.T) {
	reply := func(media string, n int) *http.Response {
		return &http.Response{
			Header: http.Header{"Content-Type": []string{media}},
			Body:   io.NopCloser(bytes.NewReader(make([]byte, n))),
		}
	}
	t.Run("a PNG comes back whole", func(t *testing.T) {
		b, media, err := picture(reply("image/png", 1024))
		if err != nil || media != "image/png" || len(b) != 1024 {
			t.Fatalf("picture() = %d bytes, %q, %v; want 1024 bytes, image/png, no error", len(b), media, err)
		}
	})
	t.Run("the parameters and case of the type are ignored", func(t *testing.T) {
		if _, media, err := picture(reply("Image/JPEG; charset=binary", 8)); err != nil || media != "image/jpeg" {
			t.Fatalf("picture() = %q, %v; want image/jpeg, no error", media, err)
		}
	})
	for _, media := range []string{"image/svg+xml", "text/html", "image/x-icon", ""} {
		t.Run("refuses "+media, func(t *testing.T) {
			if _, _, err := picture(reply(media, 8)); err == nil {
				t.Fatalf("picture(%q) succeeded, want an error", media)
			}
		})
	}
	t.Run("at the cap it is still a picture", func(t *testing.T) {
		if b, _, err := picture(reply("image/png", avatarMax)); err != nil || len(b) != avatarMax {
			t.Fatalf("picture() = %d bytes, %v; want %d bytes, no error", len(b), err, avatarMax)
		}
	})
	t.Run("one byte over the cap is refused, not cut short", func(t *testing.T) {
		if b, _, err := picture(reply("image/png", avatarMax+1)); err == nil {
			t.Fatalf("picture() returned %d bytes, want an error", len(b))
		}
	})
}
