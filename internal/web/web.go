// Package web serves the local confirmation page (127.0.0.1 only): low-confidence findings to post or drop, and
// every approval. Actions need the page's CSRF token, a same-origin request and an allowed Host header, so no other
// website can make it post or approve anything.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
)

//go:embed templates.html select-all.js running.js icon.svg favicon.png
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
}).ParseFS(files, "templates.html"))

type server struct {
	d       pipeline.Deps
	token   string
	hosts   []string
	origins []string
	head    func(repo string, number int) (string, error) // the PR's current head commit
}

func Serve(ctx context.Context, d pipeline.Deps) error {
	token, err := d.Store.CSRFToken()
	if err != nil {
		return err
	}
	s := &server{d: d, token: token, hosts: d.Cfg.AllowedHosts(), head: func(repo string, number int) (string, error) {
		pr, err := d.GH.GetPR(repo, number)
		return pr.HeadSHA, err
	}}
	for _, h := range s.hosts {
		s.origins = append(s.origins, "http://"+h)
	}
	srv := &http.Server{Addr: d.Cfg.WebAddr, Handler: s.guard(s.routes()), ReadHeaderTimeout: 5 * time.Second}
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
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("GET /pr/{owner}/{name}/{number}", s.timeline)
	mux.HandleFunc("POST /unignore", s.unignore)
	mux.HandleFunc("POST /f/{id}/wrong", s.markWrong)
	mux.HandleFunc("POST /retry", s.retry)
	mux.HandleFunc("GET /r/{id}", s.review)
	mux.HandleFunc("POST /r/{id}", s.act)
	mux.HandleFunc("GET /active.json", s.active)
	for _, name := range []string{"select-all.js", "running.js", "icon.svg", "favicon.png"} {
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
	AutoSeverity                 string
	ApproveText                  string
	Watermark                    string        // the footer the bot signs its comments with
	Stale                        string        // the PR's head, when it has moved past the reviewed commit
	Newer                        *store.Review // a later review of the same PR
	Incomplete                   bool          // a reviewer failed, so the review can't be approved
	Runs                         []store.Run
	DesignPending                []findingView // design concerns waiting for you (never posted without you)
	Assessment                   string        // the design reviewer's verdict
	Selectable                   bool          // there is something to tick and post
	Earlier                      []findingView // findings posted on the PR's earlier reviews, with what became of them
	EarlierSummary               string        // e.g. "3 fixed · 1 still open"
	OpenHigh                     int           // high-severity earlier findings a follow-up found still open
	Confirm                      bool          // approving was asked once with OpenHigh > 0: the next click approves anyway
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	waiting, err1 := s.d.Store.AwaitingYou()
	active, err2 := s.d.Store.Active()
	recent, err3 := s.d.Store.Recent(10)
	failed, err4 := s.d.Store.Failures(time.Now().Add(-retryWindow))
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The tab title carries the count, so a pinned tab shows what waits; the page refreshes every minute.
	title := "personal-review-bot"
	if len(waiting) > 0 {
		title = fmt.Sprintf("(%d) personal-review-bot", len(waiting))
	}
	s.render(w, "index", map[string]any{"Title": title, "Waiting": waiting, "Active": active, "Recent": recent,
		"Failed": failed, "Token": s.token, "Threshold": s.d.Cfg.Threshold, "AutoSeverity": s.d.Cfg.AutoSeverity})
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
	AutoSeverity               string
	FiveHour, WeekUsage        float64
	FiveHourResets, WeekResets time.Time
	HasUsage                   bool
	UsageSeen                  time.Time
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	st, now := s.d.Store, time.Now()
	p := statsPage{Threshold: s.d.Cfg.Threshold, SaveAt: s.d.Cfg.SaveAt, MinAt: s.d.Cfg.MinAt, AutoSeverity: s.d.Cfg.AutoSeverity}
	var errs [8]error
	p.Week, errs[0] = st.Totals(now.AddDate(0, 0, -7))
	p.All, errs[1] = st.Totals(time.Time{})
	p.Days, errs[2] = st.DailySpend(14, now)
	p.Reviewers, errs[3] = st.Reviewers()
	p.Since, p.Decisions, errs[4] = st.Decisions(s.d.Cfg.Threshold)
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
		Threshold: s.d.Cfg.Threshold, AutoSeverity: s.d.Cfg.AutoSeverity, ApproveText: s.d.Cfg.ApproveText,
		Watermark: s.d.Cfg.Watermark}
	if rev.Status == store.Queued || rev.Status == store.Running {
		p.Refresh = 15
	}
	if latest, err := s.d.Store.Latest(rev.Repo, rev.Number); err == nil && latest != nil && latest.ID != rev.ID {
		p.Newer = latest
	}
	if rev.Status == store.AwaitingYou && !rev.DryRun {
		p.Stale = s.moved(rev)
	}
	p.Incomplete = rev.Status == store.AwaitingYou && rev.Error != ""
	p.Runs, _ = s.d.Store.Runs(rev.ID)
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
	p.Selectable = len(p.Pending)+len(p.DesignPending) > 0
	earlier, _ := s.d.Store.EarlierPosted(rev.Repo, rev.Number, rev.ID)
	for _, f := range earlier {
		p.Earlier = append(p.Earlier, findingView{Finding: f})
	}
	p.EarlierSummary, p.OpenHigh = summarize(earlier)
	p.Confirm = r.URL.Query().Get("confirm") == "1"
	s.render(w, "review", p)
}

func (s *server) act(w http.ResponseWriter, r *http.Request) {
	rev, fs, ok := s.load(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/r/%d", rev.ID)
	if r.FormValue("action") == "rereview" {
		s.rereview(w, r, rev)
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
	var event, you string
	var post []store.Finding
	switch action {
	case "post":
		if len(selected) == 0 {
			http.Redirect(w, r, back+"?msg=Select+at+least+one+finding+to+post.", http.StatusSeeOther)
			return
		}
		event, post, you = "COMMENT", selected, fmt.Sprintf("you posted %d", len(selected))
	case "post_approve":
		event, post, you = "APPROVE", selected, fmt.Sprintf("you posted %d and approved", len(selected))
	case "approve":
		event, rest, you = "APPROVE", append(rest, selected...), "you approved"
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
			url, err := pipeline.Post(s.d, rev, event, post)
			if err != nil {
				http.Redirect(w, r, back+"?msg="+template.URLQueryEscaper("GitHub refused it: "+err.Error()), http.StatusSeeOther)
				return
			}
			for i := range post {
				post[i].PostedURL = url
			}
		}
	}
	for _, f := range post {
		f.Status = store.Posted
		_ = s.d.Store.UpdateFinding(&f)
	}
	for _, f := range rest {
		f.Status = store.Discarded
		_ = s.d.Store.UpdateFinding(&f)
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

// rereview queues the PR again at its latest commit. The review it replaces stays as it is until the new one
// finishes, so nothing is lost if the re-review fails.
func (s *server) rereview(w http.ResponseWriter, r *http.Request, rev *store.Review) {
	id, queued, err := s.requeue(rev)
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
// false). A retry of a failed review keeps its source, so it runs the way the loop meant it to (a design review only
// where the first attempt would have had one); any other re-review is yours.
func (s *server) requeue(rev *store.Review) (int64, bool, error) {
	latest, err := s.d.Store.Latest(rev.Repo, rev.Number)
	if err != nil {
		return 0, false, err
	}
	if latest != nil && (latest.Status == store.Queued || latest.Status == store.Running) {
		return latest.ID, false, nil
	}
	source, what := "manual", "re-review"
	if rev.Status == store.Failed {
		source, what = rev.Source, "retry"
	}
	id, err := s.d.Store.Insert(&store.Review{Repo: rev.Repo, Number: rev.Number, URL: rev.URL, Title: rev.Title,
		Author: rev.Author, HeadSHA: rev.HeadSHA, Status: store.Queued, Source: source, DryRun: rev.DryRun})
	if err != nil {
		return 0, false, err
	}
	s.d.Log("%s#%d: %s queued from the page", rev.Repo, rev.Number, what)
	return id, true, nil
}

// moved returns the PR's current head if it is no longer the commit rev reviewed, or "" if it is (or GitHub can't
// be asked; it will refuse a stale review itself).
func (s *server) moved(rev *store.Review) string {
	head, err := s.head(rev.Repo, rev.Number)
	if err != nil || head == "" || head == rev.HeadSHA {
		return ""
	}
	return head
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
