package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/badam1/personal-review-bot/internal/store"
)

// headData is what the shared page head needs: the tab title, and a refresh for pages that change on their own.
type headData struct {
	Title   string
	Refresh int // seconds; 0 = never
}

func page(title string, refresh int) headData { return headData{title, refresh} }

// repoName drops the owner: the bot reviews one org, and the full name wrapped every row.
func repoName(repo string) string {
	if _, name, ok := strings.Cut(repo, "/"); ok {
		return name
	}
	return repo
}

// Ordinal writes 1st, 2nd, 3rd, 4th … 11th, 12th, 13th … 21st. Shared with the TUI.
func Ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// IterationLabel labels a re-review with its number and what started it, e.g. "6th iteration · new commits"; a first
// review, a dry run or an idle skip gets "". Shared with the TUI.
func IterationLabel(r store.Review) string {
	if !r.Reran() {
		return ""
	}
	return fmt.Sprintf("%s iteration · %s", Ordinal(r.Iteration), source(r.Source))
}

// timelineURL is the page with every review of one PR.
func timelineURL(repo string, number int) string { return fmt.Sprintf("/pr/%s/%d", repo, number) }

// since is how long ago t was, as running.js keeps counting it: "45s", "7m 45s", "1h 2m".
func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// active is what runs right now, for running.js: each review's status, stage and start (Unix ms, 0 while queued).
func (s *server) active(w http.ResponseWriter, r *http.Request) {
	rs, err := s.d.Store.Active()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type item struct {
		ID      int64  `json:"id"`
		Status  string `json:"status"`
		Stage   string `json:"stage"`
		Started int64  `json:"started"`
	}
	out := []item{}
	for _, rev := range rs {
		out = append(out, item{rev.ID, rev.Status, rev.Stage, unixMilli(rev.StartedAt)})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

var fates = map[string]string{
	store.Candidate:  "⏳ verifying",
	store.AutoPosted: "🤖 bot posted",
	store.WouldPost:  "🧪 would post",
	store.Pending:    "🙋 waiting for you",
	store.Posted:     "✅ you posted",
	store.Discarded:  "🗑 you dropped",
	store.Rejected:   "✗ verifier rejected",
}

var followups = map[string]string{
	store.FollowFixed:    "✅ fixed",
	store.FollowPartly:   "◐ partly fixed",
	store.FollowOpen:     "❌ still open",
	store.FollowMoot:     "➖ no longer applies",
	store.FollowDeclined: "💬 author declined",
}

// followup says what a later review found about a posted finding.
func followup(f store.Finding) string {
	if label := followups[f.Followup]; label != "" {
		return label
	}
	return "⏳ not checked yet"
}

// fate says what became of a finding, and of its follow-up once one was done.
func fate(f store.Finding) string {
	label := fates[f.Status]
	if label == "" {
		label = f.Status
	}
	if f.Wrong {
		label += " · marked wrong"
	}
	if f.Followup != "" {
		label += " · " + followups[f.Followup]
	}
	return label
}

// summarize counts earlier findings by follow-up ("3 fixed · 1 still open"), and the high ones still open. Findings
// you marked wrong count for neither.
func summarize(fs []store.Finding) (string, int) {
	order := []string{store.FollowFixed, store.FollowPartly, store.FollowOpen, store.FollowMoot, store.FollowDeclined, ""}
	words := map[string]string{store.FollowFixed: "fixed", store.FollowPartly: "partly fixed", store.FollowOpen: "still open",
		store.FollowMoot: "no longer apply", store.FollowDeclined: "declined", "": "not checked yet"}
	counts := map[string]int{}
	openHigh := 0
	for _, f := range fs {
		if f.Wrong {
			continue
		}
		counts[f.Followup]++
		if (f.Followup == store.FollowOpen || f.Followup == store.FollowPartly) && f.Severity == "high" {
			openHigh++
		}
	}
	var parts []string
	for _, k := range order {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], words[k]))
		}
	}
	return strings.Join(parts, " · "), openHigh
}

var sources = map[string]string{
	"auto":        "requested",
	"re-request":  "re-requested",
	"new commits": "new commits",
	"activity":    "after activity",
	"manual":      "you asked",
	"idle":        "skipped: idle",
}

// source says what started a review.
func source(s string) string {
	if label := sources[s]; label != "" {
		return label
	}
	return s
}

// findingView is a finding as the review page shows it: with a link to its line on GitHub and the code around it.
type findingView struct {
	store.Finding
	URL  string
	Code []codeLine
}

type codeLine struct {
	N             int
	Added, Target bool // the PR adds the line; the finding is about it
	Text          string
}

func view(rev *store.Review, f store.Finding) findingView {
	v := findingView{Finding: f}
	if f.Path != "" && f.Line > 0 && rev.HeadSHA != "" {
		segs := strings.Split(f.Path, "/")
		for i := range segs {
			segs[i] = url.PathEscape(segs[i])
		}
		v.URL = fmt.Sprintf("https://github.com/%s/blob/%s/%s#L%d", rev.Repo, rev.HeadSHA, strings.Join(segs, "/"), f.Line)
	}
	for _, row := range strings.Split(f.Context, "\n") {
		parts := strings.SplitN(row, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		v.Code = append(v.Code, codeLine{N: n, Added: parts[1] == "+", Target: n == f.Line, Text: parts[2]})
	}
	return v
}

// wrongData feeds the "Mark as wrong" form, which needs the page's token next to the finding.
type wrongData struct {
	F     findingView
	Token string
}

func wrongForm(f findingView, token string) wrongData { return wrongData{f, token} }

type historyPage struct {
	F                   store.HistoryFilter
	Reviews, Ignored    []store.Review
	Repos, Authors      []string
	Statuses            [][2]string
	Total, Page, Pages  int
	Newer, Older, Token string
}

const historyPerPage = 50

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := historyPage{F: store.HistoryFilter{Repo: q.Get("repo"), Author: q.Get("author"), Status: q.Get("status")}, Token: s.token,
		Statuses: [][2]string{{store.AwaitingYou, "🙋 needs you"}, {store.Done, "✅ done"}, {store.Failed, "⚠️ failed"},
			{store.Ignored, "💤 skipped"}, {"active", "🔄 queued or running"}}}
	p.Page, _ = strconv.Atoi(q.Get("page"))
	p.Page = max(1, p.Page)
	var errs [4]error
	p.Reviews, p.Total, errs[0] = s.d.Store.History(p.F, historyPerPage, (p.Page-1)*historyPerPage)
	p.Repos, errs[1] = s.d.Store.Distinct("repo")
	p.Authors, errs[2] = s.d.Store.Distinct("author")
	p.Ignored, errs[3] = s.d.Store.IgnoredPRs()
	for _, err := range errs {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	p.Pages = max(1, (p.Total+historyPerPage-1)/historyPerPage)
	link := func(n int) string {
		v := url.Values{}
		for k, val := range map[string]string{"repo": p.F.Repo, "author": p.F.Author, "status": p.F.Status} {
			if val != "" {
				v.Set(k, val)
			}
		}
		v.Set("page", strconv.Itoa(n))
		return "/history?" + v.Encode()
	}
	if p.Page > 1 {
		p.Newer = link(p.Page - 1)
	}
	if p.Page < p.Pages {
		p.Older = link(p.Page + 1)
	}
	s.render(w, "history", p)
}

type timelineEntry struct {
	R        store.Review
	Findings []store.Finding
}

type timelinePage struct {
	Title   string
	Latest  store.Review
	Entries []timelineEntry
	Cost    float64
	Ignored bool
	Token   string
}

// timeline shows every review of one PR: what started it, what it found and what became of each finding.
func (s *server) timeline(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	reviews, err := s.d.Store.PRReviews(repo, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(reviews) == 0 {
		http.NotFound(w, r)
		return
	}
	p := timelinePage{Latest: reviews[0], Title: reviews[0].Title + " · personal-review-bot", Ignored: s.d.Store.IsIgnored(repo, n),
		Token: s.token}
	for _, rev := range reviews {
		fs, err := s.d.Store.Findings(rev.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.Entries = append(p.Entries, timelineEntry{R: rev, Findings: fs})
		p.Cost += rev.CostUSD
	}
	s.render(w, "timeline", p)
}

// unignore lets an ignored PR be reviewed again.
func (s *server) unignore(w http.ResponseWriter, r *http.Request) {
	repo := r.FormValue("repo")
	n, err := strconv.Atoi(r.FormValue("number"))
	if err != nil || !strings.Contains(repo, "/") {
		http.Error(w, "bad PR", http.StatusBadRequest)
		return
	}
	if err := s.d.Store.UnignorePR(repo, n); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.d.Log("%s#%d: un-ignored from the page", repo, n)
	http.Redirect(w, r, localPath(r.FormValue("back"), "/history"), http.StatusSeeOther)
}

// retryWindow is how far back the main page offers failed reviews for a retry; older ones keep the button on their
// own page.
const retryWindow = 7 * 24 * time.Hour

// retry queues failed reviews again: the one named by id, or, without one, every PR the main page lists as failed,
// oldest first. A review that is no longer failed, or whose PR is already being reviewed, is left alone.
func (s *server) retry(w http.ResponseWriter, r *http.Request) {
	var revs []store.Review
	if v := r.FormValue("id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "bad review id", http.StatusBadRequest)
			return
		}
		rev, err := s.d.Store.Get(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		revs = []store.Review{*rev}
	} else {
		var err error
		if revs, err = s.d.Store.Failures(time.Now().Add(-retryWindow)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for i := len(revs) - 1; i >= 0; i-- {
		if revs[i].Status != store.Failed {
			continue
		}
		if _, _, err := s.requeue(&revs[i], false); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, localPath(r.FormValue("back"), "/"), http.StatusSeeOther)
}

// markWrong marks a posted finding as wrong, or takes the mark back: the stats count it, and re-reviews are told.
func (s *server) markWrong(w http.ResponseWriter, r *http.Request) {
	back := localPath(r.FormValue("back"), "/")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	wrong := r.FormValue("wrong") == "1"
	if err := s.d.Store.SetWrong(id, wrong); err != nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("Not changed: "+err.Error()), http.StatusSeeOther)
		return
	}
	s.d.Log("finding %d: marked wrong = %v", id, wrong)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// localPath keeps a form's "back" address on this site, so a crafted form can't redirect anywhere else.
func localPath(p, fallback string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) {
		return fallback
	}
	return p
}
