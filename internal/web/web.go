// Package web serves the local confirmation page (127.0.0.1 only): low-confidence findings to post or drop, and
// every approval. Actions need the page's CSRF token, a same-origin request and an allowed Host header, so no other
// website can make it post or approve anything.
package web

import (
	"cmp"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/update"
	"github.com/badam1/personal-review-bot/internal/version"
)

//go:embed templates.html select-all.js filter.js running.js update.js bar.js settings.js icon.svg favicon.png
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"ago":       ago,
	"status":    StatusLabel,
	"money":     func(v float64) string { return fmt.Sprintf("$%.2f", v) },
	"pct":       func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) },
	"short":     func(sha string) string { return sha[:min(7, len(sha))] },
	"dur":       func(d time.Duration) string { return d.Round(time.Second).String() },
	"page":      page,
	"repoName":  repoName,
	"timeline":  timelineURL,
	"since":     since,
	"unixms":    unixMilli,
	"ordinal":   Ordinal,
	"iteration": IterationLabel,
	"fate":      fate,
	"followup":  followup,
	"source":    source,
	"wrongForm": wrongForm,
	"about":     func() About { return about },
	"header": func() *headerView {
		if header == nil {
			return nil
		}
		return header()
	},
	"outcome":      outcome,
	"outcomeNote":  outcomeNote,
	"sizeKind":     sizeKind,
	"certKind":     certKind,
	"followupKind": followupKind,
	"reviewSteps":  reviewSteps,
	"took":         took,
	"day":          day,
	"ticks":        ticks,
	"labelKind":    labelKind,
	"clip":         func(s string) string { return clipText(s, 90) },
	"plural":       plural,
	"initial":      initial,
	"commas":       func(s string) string { return strings.ReplaceAll(s, ",", ", ") },
	"rangeMin":     func(v string) int { n, _ := strconv.Atoi(v); return min(50, n/5*5) },
	"num": func(s setting, lo, hi int, unit string) numberField {
		return numberField{S: s, Min: lo, Max: hi, Unit: unit}
	},
	"seg": func(s setting, c []choice) segField { return segField{S: s, C: c} },
}).ParseFS(files, "templates.html"))

// numberField and segField hand a settings row's template its setting and how to show it.
type numberField struct {
	S        setting
	Min, Max int
	Unit     string
}

type segField struct {
	S setting
	C []choice
}

// initial is the first letter of a login, for an account without a picture.
func initial(login string) string {
	for _, r := range login {
		return strings.ToUpper(string(r))
	}
	return "?"
}

type server struct {
	d        pipeline.Deps
	token    string
	hosts    []string
	origins  []string
	fetch    func(repo string, number int) (gh.PR, error) // the PR as it is now: its head commit and labels
	updates  Updater                                      // nil: the page alone, which can't update
	version  string                                       // the running build's version
	threads  func(repo string, number int) ([]gh.Thread, error)
	login    string // whose reviews these are: the owner of the bot's earlier comments
	send     func(rev *store.Review, event string, fs []store.Finding, reminders []pipeline.Reminder) (string, error)
	accounts *accountCache // the GitHub account the bot acts as, kept for an hour
	listen   string        // the address the page was started on; a new ADDR takes a restart
	saving   sync.Mutex    // one Settings save at a time, so two tabs can't interleave their writes
}

// cfg is the settings as they are now: the Settings page changes them while the page runs.
func (s *server) cfg() config.Config { return s.d.Current().Cfg }

// Updater is the daemon's side of updates: install the newest release, or look for one right away. Each says what it
// did, for the page's message.
type Updater interface {
	Start() (string, error)
	CheckNow() (string, error)
}

// Serve runs the page until ctx ends. updates is the daemon's; nil (the page alone, without the daemon) leaves
// updating out.
func Serve(ctx context.Context, d pipeline.Deps, updates Updater) error {
	token, err := d.Store.CSRFToken()
	if err != nil {
		return err
	}
	about = newAbout(d.Cfg)
	if d.Live == nil {
		d.Live = config.NewLive(d.Cfg)
	}
	s := &server{d: d, token: token, accounts: &accountCache{}, hosts: d.Cfg.AllowedHosts(), listen: d.Cfg.WebAddr,
		fetch: d.GH.GetPR, updates: updates, version: version.String(), threads: d.GH.ReviewThreads, login: d.GH.Login,
		send: func(rev *store.Review, event string, fs []store.Finding, rms []pipeline.Reminder) (string, error) {
			return pipeline.Post(d.Current(), rev, event, fs, rms)
		}}
	for _, h := range s.hosts {
		s.origins = append(s.origins, "http://"+h)
	}
	header = s.headerView
	srv := &http.Server{Addr: d.Cfg.WebAddr, Handler: s.guard(s.routes()),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("POST /settings/reviewer", s.saveReviewer)
	mux.HandleFunc("POST /settings/reviewer/remove", s.removeReviewer)
	mux.HandleFunc("POST /settings/prompt", s.savePrompt)
	mux.HandleFunc("POST /settings/prompt/reset", s.resetPrompt)
	mux.HandleFunc("GET /profile", s.profile)
	mux.HandleFunc("GET /avatar", s.avatar)
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}", s.timeline)
	mux.HandleFunc("POST /unignore", s.unignore)
	mux.HandleFunc("POST /f/{id}/wrong", s.markWrong)
	mux.HandleFunc("POST /retry", s.retry)
	mux.HandleFunc("POST /update", s.startUpdate)
	mux.HandleFunc("POST /update/check", s.checkUpdate)
	mux.HandleFunc("GET /r/{id}", s.review)
	mux.HandleFunc("POST /r/{id}", s.act)
	mux.HandleFunc("GET /active.json", s.active)
	mux.HandleFunc("GET /update.json", s.updateStatus)
	for _, name := range []string{"select-all.js", "filter.js", "running.js", "update.js", "bar.js", "settings.js", "icon.svg", "favicon.png"} {
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, files, name) })
	}
	return mux
}

func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(s.hosts, r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && !slices.Contains(s.origins, o) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(s.token)) != 1 {
				http.Error(w, "missing or wrong token", http.StatusForbidden)
				return
			}
		}
		h := w.Header()
		// script-src 'self' allows only the embedded scripts; inline scripts stay blocked. connect-src 'self' lets
		// running.js read /active.json; img-src 'self' the favicon.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: with no-referrer browsers send "Origin: null" on the page's own form posts,
		// which the origin check above (rightly) refuses. Links out to GitHub still get no referrer.
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type reviewPage struct {
	Title                        string
	Refresh                      int // while the review is queued or running
	R                            *store.Review
	Pending, Auto, Rejected, Old []findingView
	Token, Msg                   string
	Threshold                    int
	ApproveText                  string
	Watermark                    string        // the footer the bot signs its comments with
	Stale                        string        // the PR's head, when it has moved past the reviewed commit
	Newer                        *store.Review // a later review of the same PR
	Incomplete                   bool          // a reviewer failed, so the review can't be approved
	Runs                         []store.Run
	DesignPending                []findingView // design concerns waiting for you (never posted without you)
	Assessment                   string        // the design reviewer's verdict
	Selectable                   bool          // there is something to tick and post
	Earlier                      []earlierView // findings posted on the PR's earlier reviews, with what became of them
	EarlierSummary               string        // e.g. "3 fixed · 1 still open"
	OpenHigh                     int           // high-severity earlier findings a follow-up found still open
	Confirm                      bool          // approving was asked once with OpenHigh > 0: the next click approves anyway
	Reminders                    int           // earlier findings you can remind the author of
	ThreadsErr                   string        // the PR's review threads couldn't be read
	EarlierBadges                []badge       // earlier findings counted by what the follow-up found
	ReviewsOfPR                  int           // reviews of this PR so far, for the timeline link
	Nav                          string        // the section it sits under: reviews or history
	Tally                        *tally        // what the bots found, once the review is over
}

// earlierView is a finding posted on an earlier review, with the reminder you can send about it.
type earlierView struct {
	findingView
	Remind *reminderView // set when the follow-up found it still open and you can remind the author on this review
}

type reminderView struct {
	Text     string // what the reminder says; you can edit it
	Ticked   bool   // nobody answered its thread, and it wasn't reminded at this commit yet
	Thread   bool   // it goes as a reply in the finding's thread, else as a line in the review body
	Resolved bool   // the thread is resolved: reminding reopens it
	Answer   string // the latest reply by someone else, e.g. "@dev: done in a follow-up PR"
	Reminded string // e.g. "you reminded the author at abc1234 (2h ago)"
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	waiting, err1 := s.d.Store.AwaitingYou()
	active, err2 := s.d.Store.Active()
	recent, err3 := s.d.Store.Recent(10)
	failed, err4 := s.d.Store.Failures(time.Now().Add(-retryWindow))
	now := time.Now()
	today, err5 := s.d.Store.Totals(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()))
	week, err6 := s.d.Store.Totals(now.AddDate(0, 0, -7))
	if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The tab title carries the count, so a pinned tab shows what waits; the page refreshes every minute.
	title := "personal-review-bot"
	if len(waiting) > 0 {
		title = fmt.Sprintf("(%d) personal-review-bot", len(waiting))
	}
	cards := make([]waitingCard, len(waiting))
	for i, rev := range waiting {
		fs, _ := s.d.Store.Findings(rev.ID)
		earlier, _ := s.d.Store.EarlierPosted(rev.Repo, rev.Number, rev.ID)
		cards[i] = waitingCard{rev, waitingBadges(rev, fs, earlier)}
	}
	s.render(w, "index", map[string]any{"Title": title, "Waiting": cards, "Active": active, "Recent": recent,
		"Failed": failed, "Token": s.token, "Threshold": s.cfg().Threshold, "AutoPost": s.cfg().AutoPostSeverities(),
		"Held": s.cfg().HeldSeverities(), "Msg": r.URL.Query().Get("msg"), "SpentToday": today.CostUSD,
		"SpentWeek": week.CostUSD})
}

// waitingCard is a review waiting for you on the main page, with what it holds.
type waitingCard struct {
	store.Review
	Badges []badge
}

// updateView is what the main page says about updates: a newer release to install, or an update under way.
type updateView struct {
	Version, URL, Current, State string
	CanInstall                   bool         // the daemon serves the page, so it can install
	Busy                         bool         // an update is installing or waiting to restart
	Done                         *update.Done // an update that finished within the hour
}

func (s *server) updateView() *updateView {
	v := &updateView{Current: s.version, State: update.State(s.d.Store), CanInstall: s.updates != nil}
	v.Busy = update.Busy(v.State)
	if rel, ok := update.Available(update.Load(s.d.Store), s.version); ok {
		v.Version, v.URL = rel.Version, rel.URL
	}
	if d, ok := update.LastDone(s.d.Store, time.Now()); ok && d.To == s.version {
		v.Done = &d
	}
	if v.Version == "" && v.State == "" && v.Done == nil {
		return nil
	}
	return v
}

// updateStatus is the update notice's live data: the version this daemon runs and what an update under way is doing.
func (s *server) updateStatus(w http.ResponseWriter, r *http.Request) {
	state := update.State(s.d.Store)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"version": s.version, "state": state, "busy": update.Busy(state)})
}

// startUpdate installs the newest release (through the daemon) and comes back to the main page saying what it does.
func (s *server) startUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		http.Error(w, "updates are installed by the daemon (personal-review-bot daemon), not the page alone", http.StatusNotFound)
		return
	}
	msg, err := s.updates.Start()
	switch {
	case err != nil:
		msg = "Not updated: " + err.Error()
	case update.Busy(update.State(s.d.Store)):
		http.Redirect(w, r, backTo(r), http.StatusSeeOther) // the version menu follows it from here
		return
	}
	http.Redirect(w, r, withMsg(backTo(r), msg), http.StatusSeeOther)
}

// checkUpdate looks for a newer release right away (instead of waiting for the hourly check) and says what it found;
// a newer one then shows with its Update button.
func (s *server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		http.Error(w, "the daemon looks for updates, not the page alone", http.StatusNotFound)
		return
	}
	msg, err := s.updates.CheckNow()
	if err != nil {
		msg = "Couldn't check: " + err.Error()
	}
	http.Redirect(w, r, withMsg(backTo(r), msg), http.StatusSeeOther)
}

// backTo is the page an action from the header came from (its path, on this site), else the main page.
func backTo(r *http.Request) string {
	if u, err := url.Parse(r.Referer()); err == nil && u.Path != "" {
		return localPath(u.Path, "/")
	}
	return "/"
}

func withMsg(path, msg string) string { return path + "?msg=" + url.QueryEscape(msg) }

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

type statsPage struct {
	Week, All                  store.Totals
	Chart                      chart
	Days                       []store.DaySpend
	Reviewers                  []store.ReviewerRow
	Since                      time.Time
	Decisions                  []store.DecisionRow
	Tiers, Models, Repos       []store.CostRow
	Threshold, SaveAt, MinAt   int
	AutoPost                   string // e.g. "low or medium"
	FiveHour, WeekUsage        float64
	FiveHourResets, WeekResets time.Time
	HasUsage                   bool
	UsageSeen                  time.Time
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	st, now := s.d.Store, time.Now()
	p := statsPage{Threshold: s.cfg().Threshold, SaveAt: s.cfg().SaveAt, MinAt: s.cfg().MinAt,
		AutoPost: s.cfg().AutoPostSeverities()}
	var errs [8]error
	p.Week, errs[0] = st.Totals(now.AddDate(0, 0, -7))
	p.All, errs[1] = st.Totals(time.Time{})
	p.Days, errs[2] = st.DailySpend(14, now)
	p.Reviewers, errs[3] = st.Reviewers()
	p.Since, p.Decisions, errs[4] = st.Decisions(s.cfg().Threshold)
	p.Tiers, errs[5] = st.Tiers()
	p.Models, errs[6] = st.Models()
	p.Repos, errs[7] = st.Repos(10)
	if err := errors.Join(errs[:]...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.Chart = spendChart(p.Days)
	if u, ok := pipeline.LoadUsage(st); ok {
		p.HasUsage, p.UsageSeen, p.FiveHourResets, p.WeekResets = true, u.Seen, u.FiveHourResets, u.WeekResets
		p.FiveHour, p.WeekUsage = u.At(now)
	}
	s.render(w, "stats", p)
}

// chart is the spend-per-day column chart, laid out here because the page runs no inline script.
type chart struct {
	Width, Height, Left, Right float64
	Top, Baseline, PlotH       float64 // the top gridline, the baseline and the height between them
	TickX, TopTickY, BaseTickY float64 // the y-axis labels
	DayY                       float64 // the x-axis labels
	TopLabel                   string
	Bars                       []bar
}

type bar struct {
	X, W, CX, LabelY float64 // slot (the hover target), its centre, the value label's y
	Path             string  // the column, rounded at its data end ("" for an empty day)
	Day, Value, Tip  string  // axis label (every other day), value label (tallest day only), tooltip
}

func spendChart(days []store.DaySpend) chart {
	c := chart{Width: 840, Height: 190, Left: 48, Right: 836, Top: 22, Baseline: 162, TickX: 40, DayY: 180}
	c.PlotH, c.TopTickY, c.BaseTickY = c.Baseline-c.Top, c.Top+4, c.Baseline+4
	peak, top := 0.0, 0
	for i, d := range days {
		if d.CostUSD > peak {
			peak, top = d.CostUSD, i
		}
	}
	scale := niceMax(peak)
	c.TopLabel = fmt.Sprintf("$%.0f", scale)
	if len(days) == 0 {
		return c
	}
	slot := (c.Right - c.Left) / float64(len(days))
	w := min(24, slot*0.6)
	for i, d := range days {
		b := bar{X: c.Left + float64(i)*slot, W: slot}
		b.CX = b.X + slot/2
		b.Tip = fmt.Sprintf("%s · $%.2f · %d review(s)", d.Day.Format("Mon Jan 2"), d.CostUSD, d.Reviews)
		if (len(days)-1-i)%2 == 0 {
			b.Day = d.Day.Format("Jan 2")
		}
		h := d.CostUSD / scale * (c.Baseline - c.Top)
		y := c.Baseline - h
		b.LabelY = y - 6
		if h >= 0.5 {
			b.Path = column(b.CX-w/2, y, w, h)
		}
		if i == top && peak > 0 {
			b.Value = fmt.Sprintf("$%.2f", d.CostUSD)
		}
		c.Bars = append(c.Bars, b)
	}
	return c
}

// column draws a bar from the baseline up, square at the bottom and rounded (4px) at the top.
func column(x, y, w, h float64) string {
	r := min(4, h, w/2)
	return fmt.Sprintf("M%.1f,%.1f V%.1f Q%.1f,%.1f %.1f,%.1f H%.1f Q%.1f,%.1f %.1f,%.1f V%.1f Z",
		x, y+h, y+r, x, y, x+r, y, x+w-r, x+w, y, x+w, y+r, y+h)
}

// niceMax rounds a peak up to 1, 2, 2.5 or 5 times a power of ten, for the top gridline.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

func (s *server) load(w http.ResponseWriter, r *http.Request) (*store.Review, []store.Finding, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	rev, err := s.d.Store.Get(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	fs, err := s.d.Store.Findings(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	return rev, fs, true
}

func (s *server) review(w http.ResponseWriter, r *http.Request) {
	rev, fs, ok := s.load(w, r)
	if !ok {
		return
	}
	p := reviewPage{Title: rev.Title + " · personal-review-bot", R: rev, Token: s.token, Msg: r.URL.Query().Get("msg"),
		Threshold: s.cfg().Threshold, ApproveText: s.cfg().ApproveText,
		Watermark: s.cfg().Watermark}
	if rev.Status == store.Queued || rev.Status == store.Running {
		p.Refresh = 15
	}
	if latest, err := s.d.Store.Latest(rev.Repo, rev.Number); err == nil && latest != nil && latest.ID != rev.ID {
		p.Newer = latest
	}
	p.Nav = home(*rev, p.Newer != nil)
	if rev.Status == store.AwaitingYou && !rev.DryRun {
		p.Stale = s.moved(rev)
	}
	p.Incomplete = rev.Status == store.AwaitingYou && rev.Error != ""
	p.Runs, _ = s.d.Store.Runs(rev.ID)
	slices.SortStableFunc(p.Runs, func(a, b store.Run) int { return cmp.Compare(runOrder(a.Agent), runOrder(b.Agent)) })
	if rev.Status != store.Queued && rev.Status != store.Running && (len(fs) > 0 || len(p.Runs) > 0) {
		t := tallyFindings(fs)
		p.Tally = &t
	}
	for _, run := range p.Runs {
		if run.Agent == agents.DesignAgent && run.Error == "" {
			p.Assessment = run.Checked
		}
	}
	for _, f := range fs {
		v := view(rev, f)
		if f.Agent == agents.DesignAgent && f.Status == store.Pending {
			p.DesignPending = append(p.DesignPending, v)
			continue
		}
		switch f.Status {
		case store.Pending:
			p.Pending = append(p.Pending, v)
		case store.AutoPosted, store.WouldPost:
			p.Auto = append(p.Auto, v)
		case store.Rejected:
			p.Rejected = append(p.Rejected, v)
		default: // posted / discarded by you
			p.Old = append(p.Old, v)
		}
	}
	earlier, _ := s.d.Store.EarlierPosted(rev.Repo, rev.Number, rev.ID)
	var threads []gh.Thread
	canRemind := rev.Status == store.AwaitingYou && !rev.DryRun && slices.ContainsFunc(earlier, pipeline.Remindable)
	if canRemind {
		var err error
		if threads, err = s.threads(rev.Repo, rev.Number); err != nil {
			p.ThreadsErr = err.Error()
		}
	}
	for _, f := range earlier {
		v := earlierView{findingView: findingView{Finding: f}}
		if canRemind && pipeline.Remindable(f) {
			v.Remind = s.reminder(rev, f, threads)
			p.Reminders++
		}
		p.Earlier = append(p.Earlier, v)
	}
	p.Selectable = len(p.Pending)+len(p.DesignPending)+p.Reminders > 0
	p.EarlierSummary, p.OpenHigh = summarize(earlier)
	p.EarlierBadges = earlierBadges(earlier)
	slices.SortStableFunc(p.Earlier, func(a, b earlierView) int { // the ones you can act on first
		return cmp.Compare(btoi(b.Remind != nil), btoi(a.Remind != nil))
	})
	if all, err := s.d.Store.PRReviews(rev.Repo, rev.Number); err == nil {
		p.ReviewsOfPR = len(all)
	}
	p.Confirm = r.URL.Query().Get("confirm") == "1"
	s.render(w, "review", p)
}

var (
	decidingMu sync.Mutex
	deciding   = map[int64]*sync.Mutex{}
)

// decideLock serializes the decisions taken on one review. Without it two submits of the page's form — a double
// click, a request the browser retried — both read the review as still waiting for you and both post to GitHub,
// which is two reviews on someone's PR under your name. The store claims a queued review the same way (ClaimNext).
func decideLock(id int64) *sync.Mutex {
	decidingMu.Lock()
	defer decidingMu.Unlock()
	if deciding[id] == nil {
		deciding[id] = &sync.Mutex{}
	}
	return deciding[id]
}

func (s *server) act(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	l := decideLock(id)
	l.Lock()
	defer l.Unlock()

	// Read the review inside the lock: its status is what says the decision is still open.
	rev, fs, ok := s.load(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/r/%d", rev.ID)
	if a := r.FormValue("action"); a == "rereview" || a == "deep" {
		s.rereview(w, r, rev, a == "deep")
		return
	}
	if rev.Status != store.AwaitingYou {
		http.Redirect(w, r, back+"?msg=Already+decided.", http.StatusSeeOther)
		return
	}
	var selected, rest []store.Finding
	auto := 0
	for _, f := range fs {
		switch {
		case f.Status == store.AutoPosted || f.Status == store.WouldPost:
			auto++
		case f.Status != store.Pending:
		case r.FormValue(fmt.Sprintf("f_%d", f.ID)) == "on":
			if c := strings.TrimSpace(r.FormValue(fmt.Sprintf("c_%d", f.ID))); c != "" {
				f.Comment = c
			}
			selected = append(selected, f)
		default:
			rest = append(rest, f)
		}
	}

	action := r.FormValue("action")
	var reminders []pipeline.Reminder
	if action == "post" || action == "post_approve" {
		reminders = s.tickedReminders(r, rev)
	}
	var event, you string
	var post []store.Finding
	switch action {
	case "post":
		if len(selected)+len(reminders) == 0 {
			http.Redirect(w, r, back+"?msg=Select+at+least+one+finding+or+reminder+to+post.", http.StatusSeeOther)
			return
		}
		event, post, you = "COMMENT", selected, posted(len(selected), len(reminders))
	case "post_approve":
		event, post, you = "APPROVE", selected, posted(len(selected), len(reminders))+" and approved"
	case "approve":
		event, rest, you = "APPROVE", append(rest, selected...), "approved by you"
	case "skip":
		rest, you = append(rest, selected...), "you skipped it"
	case "ignore":
		rest, you = append(rest, selected...), "you ignored this PR"
		if err := s.d.Store.IgnorePR(rev.Repo, rev.Number); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	if event == "APPROVE" && rev.Error != "" {
		msg := "Not approved: some reviewers failed, so this review is incomplete. Re-review it first."
		http.Redirect(w, r, back+"?msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
		return
	}
	// A high finding posted earlier and still open asks once before approving; the next click approves anyway.
	if event == "APPROVE" && r.FormValue("anyway") != "1" {
		earlier, _ := s.d.Store.EarlierPosted(rev.Repo, rev.Number, rev.ID)
		if _, openHigh := summarize(earlier); openHigh > 0 {
			msg := fmt.Sprintf("%d high-severity finding(s) posted earlier are still open (see Earlier findings). Click again to approve anyway.", openHigh)
			http.Redirect(w, r, back+"?confirm=1&msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
			return
		}
	}
	if event != "" {
		if rev.DryRun {
			you = "dry run: " + you + " (nothing sent to GitHub)"
		} else {
			// GitHub refuses a review pinned to a commit that is no longer the head, and the findings' lines were
			// mapped against the old diff anyway.
			if head := s.moved(rev); head != "" {
				msg := fmt.Sprintf("Not sent: the PR moved from %.7s to %.7s since this review. Re-review it first.", rev.HeadSHA, head)
				http.Redirect(w, r, back+"?msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
				return
			}
			url, err := s.send(rev, event, post, reminders)
			// Nothing was posted only when it also failed: an empty URL on its own means GitHub took the review
			// without saying where it is, and treating that as a failure left the findings unrecorded here while
			// they were live on the PR — and read err, which is nil, for a reason.
			if err != nil && url == "" {
				http.Redirect(w, r, back+"?msg="+template.URLQueryEscaper("GitHub refused it: "+err.Error()), http.StatusSeeOther)
				return
			}
			if err != nil { // posted, with a part that didn't go through: say so on the page
				back += "?msg=" + template.URLQueryEscaper("Posted, but: "+err.Error())
			}
			for i := range post {
				post[i].PostedURL = url
			}
			for _, rm := range reminders {
				if err := s.d.Store.SetReminded(rm.Finding.ID, rev.HeadSHA); err != nil {
					s.d.Log("%s#%d: reminded of finding %d but couldn't record it, it may be sent again: %v",
						rev.Repo, rev.Number, rm.Finding.ID, err)
				}
			}
		}
	}
	// A finding that is on the PR but still "pending" here would be offered again on the next review, so a
	// store that refuses the update is worth a line in the log rather than nothing.
	for _, f := range post {
		f.Status = store.Posted
		if err := s.d.Store.UpdateFinding(&f); err != nil {
			s.d.Log("%s#%d: posted finding %d but couldn't record it, it may be posted again: %v",
				rev.Repo, rev.Number, f.ID, err)
		}
	}
	for _, f := range rest {
		f.Status = store.Discarded
		if err := s.d.Store.UpdateFinding(&f); err != nil {
			s.d.Log("%s#%d: couldn't record finding %d as discarded: %v", rev.Repo, rev.Number, f.ID, err)
		}
	}
	decision := you
	if auto > 0 {
		decision = fmt.Sprintf("bot posted %d, %s", auto, you)
	}
	if err := s.d.Store.Finish(rev.ID, store.Done, decision, "", rev.Error); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.d.Log("%s#%d: %s", rev.Repo, rev.Number, decision)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// posted says what you posted, e.g. "you posted 2 and reminded of 1".
func posted(findings, reminders int) string {
	switch {
	case reminders == 0:
		return fmt.Sprintf("you posted %d", findings)
	case findings == 0:
		return fmt.Sprintf("you reminded of %d still open", reminders)
	}
	return fmt.Sprintf("you posted %d and reminded of %d still open", findings, reminders)
}

// reminder is what the page offers for a still-open earlier finding: its thread's state and the text to post.
func (s *server) reminder(rev *store.Review, f store.Finding, threads []gh.Thread) *reminderView {
	rm := &reminderView{Text: pipeline.ReminderText(f, rev.HeadSHA), Ticked: f.RemindedSHA != rev.HeadSHA}
	if t, ok := pipeline.MatchThread(f, threads, s.login, s.cfg().Watermark); ok {
		rm.Thread, rm.Resolved = true, t.Resolved
		if a, ok := pipeline.Answer(t, s.login); ok {
			body := strings.Join(strings.Fields(a.Body), " ")
			if r := []rune(body); len(r) > 240 {
				body = string(r[:240]) + "…"
			}
			rm.Answer, rm.Ticked = "@"+a.Author+": "+body, false
		}
	}
	if f.RemindedSHA != "" {
		rm.Reminded = fmt.Sprintf("you reminded the author at %.7s (%s)", f.RemindedSHA, ago(f.RemindedAt))
	}
	return rm
}

// tickedReminders are the reminders ticked on the page, each with its edited text and, when it was posted inline,
// its thread. If the threads can't be read, the reminders go in the review body.
func (s *server) tickedReminders(r *http.Request, rev *store.Review) []pipeline.Reminder {
	if rev.DryRun {
		return nil
	}
	earlier, _ := s.d.Store.EarlierPosted(rev.Repo, rev.Number, rev.ID)
	var out []pipeline.Reminder
	for _, f := range earlier {
		if !pipeline.Remindable(f) || r.FormValue(fmt.Sprintf("rm_%d", f.ID)) != "on" {
			continue
		}
		text := strings.TrimSpace(r.FormValue(fmt.Sprintf("rt_%d", f.ID)))
		if text == "" {
			text = pipeline.ReminderText(f, rev.HeadSHA)
		}
		out = append(out, pipeline.Reminder{Finding: f, Text: text})
	}
	if len(out) == 0 {
		return nil
	}
	threads, err := s.threads(rev.Repo, rev.Number)
	if err != nil {
		s.d.Log("%s#%d: review threads unavailable, reminders go in the review body: %v", rev.Repo, rev.Number, err)
	}
	for i := range out {
		if t, ok := pipeline.MatchThread(out[i].Finding, threads, s.login, s.cfg().Watermark); ok {
			out[i].Thread = &t
		}
	}
	return out
}

// rereview queues the PR again at its latest commit, as a deep review if you asked for one. The review it replaces
// stays as it is until the new one finishes, so nothing is lost if the re-review fails.
func (s *server) rereview(w http.ResponseWriter, r *http.Request, rev *store.Review, deep bool) {
	id, queued, err := s.requeue(rev, deep)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !queued {
		http.Redirect(w, r, fmt.Sprintf("/r/%d?msg=Already+being+reviewed.", id), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/r/%d", id), http.StatusSeeOther)
}

// requeue queues a new review of rev's PR and returns its id, or the id of the review already queued or running (and
// false). A retry of a failed review keeps its source and depth, so it runs the way it was meant to (a design review
// only where the first attempt would have had one); any other re-review is yours, deep if you ask.
func (s *server) requeue(rev *store.Review, deep bool) (int64, bool, error) {
	latest, err := s.d.Store.Latest(rev.Repo, rev.Number)
	if err != nil {
		return 0, false, err
	}
	if latest != nil && (latest.Status == store.Queued || latest.Status == store.Running) {
		return latest.ID, false, nil
	}
	source, what := "manual", "re-review"
	if rev.Status == store.Failed {
		source, what, deep = rev.Source, "retry", deep || rev.Deep
	}
	if deep {
		what = "deep " + what
	}
	id, err := s.d.Store.Insert(&store.Review{Repo: rev.Repo, Number: rev.Number, URL: rev.URL, Title: rev.Title,
		Author: rev.Author, HeadSHA: rev.HeadSHA, Status: store.Queued, Source: source, DryRun: rev.DryRun, Deep: deep})
	if err != nil {
		return 0, false, err
	}
	s.d.Log("%s#%d: %s queued from the page", rev.Repo, rev.Number, what)
	return id, true, nil
}

// moved returns the PR's current head if it is no longer the commit rev reviewed, or "" if it is (or GitHub can't
// be asked; it will refuse a stale review itself).
// It also brings the labels up to date, since a labeler may add one after the review started.
func (s *server) moved(rev *store.Review) string {
	pr, err := s.fetch(rev.Repo, rev.Number)
	if err != nil {
		return ""
	}
	if labels := s.cfg().Shown(pr.Labels); !slices.Equal(labels, rev.Labels) {
		_ = s.d.Store.SetLabels(rev.ID, labels)
		rev.Labels = labels
	}
	if pr.HeadSHA == "" || pr.HeadSHA == rev.HeadSHA {
		return ""
	}
	return pr.HeadSHA
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Local().Format("Jan 2 15:04")
	}
}

// StatusLabel is shared with the TUI.
func StatusLabel(r store.Review) string {
	switch r.Status {
	case store.Queued:
		return "⏳ queued"
	case store.Running:
		return "🔄 " + r.Stage
	case store.AwaitingYou:
		return "🙋 needs you"
	case store.Done:
		return "✅ done"
	case store.Failed:
		return "⚠️ failed"
	case store.Ignored:
		return "💤 skipped"
	}
	return r.Status
}
