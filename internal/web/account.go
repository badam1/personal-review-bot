package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/badam1/personal-review-bot/internal/gh"
)

// accountView is the Settings page's Account section: who every comment and approval goes out as.
type accountView struct {
	Login, Name, URL string
	Avatar           bool      // the picture came down, so /avatar serves it
	Orgs             []orgView // the organisations the account belongs to
	NotMember        string    // the watched orgs the account isn't among the members of
	Err              string    // GitHub couldn't be asked; the login from start-up is still known
}

type orgView struct {
	Name    string
	Watched bool // the org the bot reviews PRs in
}

// accountCache keeps the account and its picture for an hour. The profile page would otherwise ask GitHub twice
// on every load, and neither a display name nor a picture changes often enough to be worth that.
type accountCache struct {
	mu      sync.Mutex
	acc     gh.Account
	pic     []byte
	picType string
	at      time.Time
	err     error
}

const accountTTL = time.Hour

// get returns the account, asking GitHub at most once an hour. A failed attempt is kept too, so a page reloaded
// while GitHub is unreachable doesn't try again on every refresh.
func (a *accountCache) get(ctx context.Context, c *gh.Client) (gh.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.at.IsZero() && time.Since(a.at) < accountTTL {
		return a.acc, a.err
	}
	a.at = time.Now()
	a.acc, a.err = c.Account()
	a.pic, a.picType = nil, ""
	if a.err != nil {
		return a.acc, a.err
	}
	if a.acc.AvatarURL != "" {
		if pic, media, err := gh.Avatar(ctx, a.acc.AvatarURL); err == nil {
			a.pic, a.picType = pic, media
		}
	}
	return a.acc, nil
}

func (a *accountCache) picture() ([]byte, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pic, a.picType
}

// account is the Account section, read from the cache (so GitHub is asked at most once an hour). It takes the
// watched orgs one by one, since ORG holds a list: every one of them is marked, and each is matched ignoring
// case, because GitHub's org: search does too.
func (s *server) account(ctx context.Context, orgs ...string) accountView {
	v := accountView{Login: s.login}
	acc, err := s.accounts.get(ctx, s.d.GH)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	v.Login, v.Name, v.URL = acc.Login, acc.Name, acc.HTMLURL
	pic, _ := s.accounts.picture()
	v.Avatar = len(pic) > 0
	watched := func(name string) bool {
		return slices.ContainsFunc(orgs, func(o string) bool { return o != "" && strings.EqualFold(o, name) })
	}
	for _, o := range acc.Orgs {
		v.Orgs = append(v.Orgs, orgView{Name: o, Watched: watched(o)})
	}
	if len(acc.Orgs) > 0 { // no orgs at all: gh may lack read:org, so it can't tell
		var missing []string
		for _, o := range orgs {
			if o != "" && !slices.ContainsFunc(acc.Orgs, func(m string) bool { return strings.EqualFold(m, o) }) {
				missing = append(missing, o)
			}
		}
		v.NotMember = strings.Join(missing, ", ")
	}
	return v
}

// profile was the account's own page; it is the Settings page's Account section now.
func (s *server) profile(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings#account", http.StatusMovedPermanently)
}

// avatar serves the account's picture from the bot itself, so the page keeps img-src 'self' and the browser
// never asks GitHub for anything while you look at your settings.
func (s *server) avatar(w http.ResponseWriter, r *http.Request) {
	if _, err := s.accounts.get(r.Context(), s.d.GH); err != nil {
		http.NotFound(w, r)
		return
	}
	pic, media := s.accounts.picture()
	if len(pic) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", media)
	// The bytes come from GitHub: serve them as the media type they arrived as and nothing else.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(pic)
}
