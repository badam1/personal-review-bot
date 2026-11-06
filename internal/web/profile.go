package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
)

// profilePage is who the bot acts as, and the rules it follows under that name. Everything it posts and every
// approval it sends goes out as this account, so the page puts the account and those rules in one place.
type profilePage struct {
	Account   gh.Account
	HasAvatar bool   // the picture came down, so the page can show it
	Err       string // GitHub couldn't be asked; the settings below are still worth showing
	Watched   string // the org being watched ("" = any)
	Member    bool   // the account belongs to the org it watches
	Scoped    bool   // an org is set at all

	Threshold                int
	AutoPost, Held           string
	Watermark, ApproveText   string
	Poll                     time.Duration
	Parallel                 int
	MaxAgeDays               int
	Today, Week, All         store.Totals
	HasUsage                 bool
	FiveHour, Week7          float64
	FiveHourReset, WeekReset time.Time
	UsageSeen                time.Time
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

func (s *server) profile(w http.ResponseWriter, r *http.Request) {
	cfg := s.d.Cfg
	p := profilePage{Watched: cfg.Org, Scoped: cfg.Org != "", Threshold: cfg.Threshold,
		AutoPost: cfg.AutoPostSeverities(), Held: cfg.HeldSeverities(), Watermark: cfg.Watermark,
		ApproveText: cfg.ApproveText, Poll: cfg.PollInterval, Parallel: cfg.MaxParallel,
		MaxAgeDays: int(cfg.MaxAge / (24 * time.Hour))}

	acc, err := s.accounts.get(r.Context(), s.d.GH)
	switch {
	case err != nil:
		p.Err = err.Error()
		p.Account = gh.Account{Login: s.login} // the login is known from start-up even when the rest isn't
	default:
		p.Account = acc
		pic, _ := s.accounts.picture()
		p.HasAvatar = len(pic) > 0
		p.Member = p.Scoped && slices.ContainsFunc(acc.Orgs, func(o string) bool {
			return strings.EqualFold(o, cfg.Org)
		})
	}

	now := time.Now()
	st := s.d.Store
	var errs [3]error
	p.Today, errs[0] = st.Totals(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()))
	p.Week, errs[1] = st.Totals(now.AddDate(0, 0, -7))
	p.All, errs[2] = st.Totals(time.Time{})
	if err := errors.Join(errs[:]...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u, ok := pipeline.LoadUsage(st); ok {
		p.HasUsage, p.UsageSeen, p.FiveHourReset, p.WeekReset = true, u.Seen, u.FiveHourResets, u.WeekResets
		p.FiveHour, p.Week7 = u.At(now)
	}
	s.render(w, "profile", p)
}

// avatar serves the account's picture from the bot itself, so the page keeps img-src 'self' and the browser
// never asks GitHub for anything while you look at your own profile.
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
