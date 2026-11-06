package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
)

func TestFooter(t *testing.T) {
	cfg := config.Config{Watermark: "🤖 acme-bot"}
	if got := cfg.Footer(92); got != "<sub>🤖 acme-bot · 92%</sub>" {
		t.Errorf("footer = %q", got)
	}
}

// The confirmation page must refuse anything another website (or a DNS-rebinding page) could send.
func TestGuard(t *testing.T) {
	s := &server{token: "secret", hosts: []string{"127.0.0.1:12346", "localhost:12346"},
		origins: []string{"http://127.0.0.1:12346", "http://localhost:12346"}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.guard(ok)

	post := func(host, origin, token string) int {
		form := url.Values{"csrf": {token}, "action": {"approve"}}
		req := httptest.NewRequest(http.MethodPost, "http://"+host+"/r/1", strings.NewReader(form.Encode()))
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cases := []struct {
		name, host, origin, token string
		want                      int
	}{
		{"own page", "localhost:12346", "http://localhost:12346", "secret", 200},
		{"no origin header, right token", "127.0.0.1:12346", "", "secret", 200},
		{"other website", "localhost:12346", "https://evil.example", "secret", 403},
		{"missing token", "localhost:12346", "http://localhost:12346", "", 403},
		{"wrong token", "localhost:12346", "http://localhost:12346", "guess", 403},
		{"DNS rebinding host", "evil.example:12346", "", "secret", 403},
	}
	for _, c := range cases {
		if got := post(c.host, c.origin, c.token); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost:12346/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame protection: %q", csp)
	}
	// Browsers send "Origin: null" on form posts from a no-referrer page, which the guard would refuse.
	if rp := rec.Header().Get("Referrer-Policy"); rp != "same-origin" {
		t.Errorf("Referrer-Policy = %q; must be same-origin so the page's own posts carry its origin", rp)
	}
	if got := post("localhost:12346", "null", "secret"); got != 403 {
		t.Errorf("Origin: null must stay forbidden, got %d", got)
	}
}

// A review of a commit that is no longer the PR's head must not reach GitHub, and the page must offer a re-review.
func TestStaleReview(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.Insert(&store.Review{Repo: "o/r", Number: 7, URL: "https://github.com/o/r/pull/7", HeadSHA: "aaaaaaa1",
		Status: store.AwaitingYou})
	if err != nil {
		t.Fatal(err)
	}
	head := "bbbbbbb2"
	// d.GH stays nil: anything that tried to post would panic.
	s := &server{d: pipeline.Deps{Store: st, Log: t.Logf}, token: "secret",
		fetch: func(string, int) (gh.PR, error) { return gh.PR{HeadSHA: head}, nil }}

	do := func(method, action string) *httptest.ResponseRecorder {
		form := ""
		if action != "" {
			form = url.Values{"csrf": {"secret"}, "action": {action}}.Encode()
		}
		req := httptest.NewRequest(method, fmt.Sprintf("/r/%d", id), strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", fmt.Sprint(id))
		rec := httptest.NewRecorder()
		if method == http.MethodGet {
			s.review(rec, req)
		} else {
			s.act(rec, req)
		}
		return rec
	}

	page := do(http.MethodGet, "").Body.String()
	if !strings.Contains(page, "New commits since this review") || !strings.Contains(page, `value="rereview"`) {
		t.Errorf("stale page should explain and offer a re-review:\n%s", page)
	}
	if !strings.Contains(page, `value="approve" disabled`) {
		t.Error("approve button should be disabled while the review is stale")
	}

	rec := do(http.MethodPost, "approve")
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "msg=") {
		t.Errorf("stale approve should redirect back with a message, got %q", loc)
	}
	if r, _ := st.Get(id); r.Status != store.AwaitingYou {
		t.Errorf("stale approve changed the review to %s", r.Status)
	}

	rec = do(http.MethodPost, "rereview")
	latest, _ := st.Latest("o/r", 7)
	if latest.ID == id || latest.Status != store.Queued {
		t.Fatalf("re-review should queue a new review, latest is %d (%s)", latest.ID, latest.Status)
	}
	if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/r/%d", latest.ID) {
		t.Errorf("re-review should redirect to the new review, got %q", loc)
	}
	if r, _ := st.Get(id); r.Status != store.AwaitingYou {
		t.Errorf("the old review should wait until the re-review finishes, it is %s", r.Status)
	}
	if rec := do(http.MethodPost, "rereview"); !strings.Contains(rec.Header().Get("Location"), "Already") {
		t.Error("a second re-review while one is queued should not queue another")
	}

	head = "aaaaaaa1"
	if page := do(http.MethodGet, "").Body.String(); strings.Contains(page, "New commits since this review") {
		t.Error("page claims new commits when the head is the reviewed commit")
	}
}

// When a reviewer failed, the page shows what each reviewer did and offers a re-review, never an approval.
func TestIncompleteReview(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 7, HeadSHA: "aaa", Status: store.Running})
	_ = st.Finish(id, store.AwaitingYou, "1 of 2 reviewers failed: re-review", "", "security: claude (opus) timed out")
	_ = st.AddRun(&store.Run{ReviewID: id, Agent: "logic", Model: "opus", Turns: 21, Denials: 2,
		Checked: "Traced every caller of promote()"})
	_ = st.AddRun(&store.Run{ReviewID: id, Agent: "security", Model: "opus", Error: "claude (opus) timed out"})
	// d.GH stays nil: anything that tried to post would panic.
	s := &server{d: pipeline.Deps{Store: st, Log: t.Logf}, token: "secret",
		fetch: func(string, int) (gh.PR, error) { return gh.PR{HeadSHA: "aaa"}, nil }}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/r/%d", id), nil)
	req.SetPathValue("id", fmt.Sprint(id))
	rec := httptest.NewRecorder()
	s.review(rec, req)
	page := rec.Body.String()
	for _, want := range []string{`value="approve" disabled`, `class="btn primary big" form="decide" name="action" value="rereview"`,
		"can't be approved", "nothing found", "Traced every caller of promote()", "2 tool calls outside its read-only tools", "failed"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}

	form := url.Values{"csrf": {"secret"}, "action": {"approve"}}.Encode()
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/r/%d", id), strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", fmt.Sprint(id))
	rec = httptest.NewRecorder()
	s.act(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "Not+approved") && !strings.Contains(loc, "Not%20approved") {
		t.Errorf("approving an incomplete review should be refused, got redirect %q", loc)
	}
	if r, _ := st.Get(id); r.Status != store.AwaitingYou {
		t.Errorf("refused approval changed the review to %s", r.Status)
	}
}

// The main page shows what needs you and the latest reviews; the score table lives on the stats page.
func TestIndex(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{d: pipeline.Deps{Store: st, Cfg: config.Config{Threshold: 80, AutoPost: []string{"low", "medium"}}}}
	rec := httptest.NewRecorder()
	s.index(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	page := rec.Body.String()
	for _, want := range []string{`href="/stats"`, "80% certainty or more and of low or medium severity post on their own",
		"findings of high severity, design concerns and every approval wait", "Needs you", ">Recent</h2>", "Nothing waiting for you."} {
		if !strings.Contains(page, want) {
			t.Errorf("index is missing %q", want)
		}
	}
	if strings.Contains(page, "What became of findings") {
		t.Error("the score table moved to the stats page")
	}
}

// The stats page sums up reviews, reviewers, spend and the score table, and charts spend per day.
func TestStats(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 7, HeadSHA: "aaa", Status: store.Running})
	_ = st.SetPlan(id, "t", "a", "aaa", "large", "opus → opus", "900 lines in 3 files", 900, 10, 3)
	_ = st.AddCost(id, 4.5)
	_ = st.AddRun(&store.Run{ReviewID: id, Agent: "logic", Model: "opus", Findings: 1, CostUSD: 1.5})
	_ = st.AddRun(&store.Run{ReviewID: id, Agent: "verifier", Model: "opus", Findings: 1, CostUSD: 0.5})
	fid, _ := st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "high", Title: "t", Comment: "c", Status: store.Candidate})
	_ = st.UpdateFinding(&store.Finding{ID: fid, Comment: "c", Confidence: 90, Verdict: "confirmed", Status: store.AutoPosted})
	_ = st.Finish(id, store.Done, "posted 1, you approved", "", "")

	s := &server{d: pipeline.Deps{Store: st, Cfg: config.Config{Threshold: 80, AutoPost: []string{"low", "medium"}, SaveAt: 75, MinAt: 90}}}
	rec := httptest.NewRecorder()
	s.stats(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{"Last 7 days", "$4.50", "Spend per day · last 14 days", `class="bar"`, "As a table",
		"Reviewers · all time", "<b>logic</b>", "checks the others' findings", "What became of findings, by score",
		"Spend by PR size", "Spend by model", "o/r"} {
		if !strings.Contains(page, want) {
			t.Errorf("stats page is missing %q", want)
		}
	}
}

// Columns rise from the baseline with a rounded top; the axis tops out at a round number; only the tallest day is
// labelled, and every other day on the axis, today included.
func TestSpendChart(t *testing.T) {
	for v, want := range map[float64]float64{0: 1, 0.4: 0.5, 7: 10, 12: 20, 23: 25, 41: 50, 250: 250} {
		if got := niceMax(v); got != want {
			t.Errorf("niceMax(%v) = %v, want %v", v, got, want)
		}
	}
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	days := []store.DaySpend{{Day: day, CostUSD: 10}, {Day: day.AddDate(0, 0, 1)}, {Day: day.AddDate(0, 0, 2), CostUSD: 40, Reviews: 3}}
	c := spendChart(days)
	if c.TopLabel != "$50" || len(c.Bars) != 3 {
		t.Fatalf("chart = %+v", c)
	}
	if c.Bars[1].Path != "" || c.Bars[0].Path == "" || c.Bars[0].Value != "" || c.Bars[2].Value != "$40.00" {
		t.Errorf("bars = %+v", c.Bars)
	}
	if c.Bars[2].Day != "Nov 3" || c.Bars[1].Day != "" || c.Bars[0].Day != "Nov 1" {
		t.Errorf("axis labels = %q %q %q", c.Bars[0].Day, c.Bars[1].Day, c.Bars[2].Day)
	}
	if !strings.Contains(c.Bars[2].Tip, "Tue Nov 3 · $40.00 · 3 review(s)") {
		t.Errorf("tooltip = %q", c.Bars[2].Tip)
	}
}

// Design concerns show in their own section with the assessment, can be ticked, and don't read as "no issues".
func TestDesignSection(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 7, HeadSHA: "aaa", Status: store.Running})
	_ = st.Finish(id, store.AwaitingYou, "1 need you", "", "")
	_ = st.AddRun(&store.Run{ReviewID: id, Agent: "design", Model: "opus", Findings: 1,
		Checked: "Compared against ADR-0009 and the sibling stacks; the layout fits."})
	fid, _ := st.AddFinding(&store.Finding{ReviewID: id, Agent: "design", Severity: "medium",
		Title: "Second release path", Comment: "Use the shared workflow.", Confidence: 85, Status: store.Pending})
	s := &server{d: pipeline.Deps{Store: st, Log: t.Logf}, token: "secret",
		fetch: func(string, int) (gh.PR, error) { return gh.PR{HeadSHA: "aaa"}, nil }}
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/r/%d", id), nil)
	req.SetPathValue("id", fmt.Sprint(id))
	rec := httptest.NewRecorder()
	s.review(rec, req)
	page := rec.Body.String()
	for _, want := range []string{`Design concerns <span class="count design">1</span>`, "Never posted without you.", fmt.Sprintf(`name="f_%d"`, fid),
		"Compared against ADR-0009", "1 concern</span>", `value="post"`, "Approve without comments", `<b class="cert hi">85%</b> sure`} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(page, "No issues found") {
		t.Error(`a PR with a design concern must not read as "No issues found"`)
	}
}

// With findings to tick, the page offers Select all, served as a same-origin script the CSP allows; inline stays blocked.
func TestSelectAll(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 7, HeadSHA: "aaa", Status: store.Running})
	_ = st.Finish(id, store.AwaitingYou, "2 need you", "", "")
	for _, title := range []string{"Nil map write", "Unchecked error"} {
		_, _ = st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "medium", Path: "main.go", Line: 3,
			Title: title, Comment: "Fix it.", Status: store.Pending})
	}
	s := &server{d: pipeline.Deps{Store: st, Log: t.Logf}, token: "secret", hosts: []string{"localhost:12346"},
		fetch: func(string, int) (gh.PR, error) { return gh.PR{HeadSHA: "aaa"}, nil }}
	h := s.guard(s.routes())
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346"+path, nil))
		return rec
	}

	rec := get(fmt.Sprintf("/r/%d", id))
	page := rec.Body.String()
	for _, want := range []string{`id="select-all" hidden`, `<script src="/select-all.js" defer>`} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self';") {
		t.Errorf("CSP should allow only same-origin scripts: %q", csp)
	}

	js := get("/select-all.js")
	if js.Code != http.StatusOK || !strings.Contains(js.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(js.Body.String(), `getElementById("select-all")`) {
		t.Errorf("select-all.js: status %d, type %q", js.Code, js.Header().Get("Content-Type"))
	}
}

// The profile page says who the bot acts as and the rules it follows under that name. The account is seeded into
// the cache so the page renders without asking GitHub, which is also what a second load within the hour does.
func TestProfile(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.Insert(&store.Review{Repo: "acme/api", Number: 7, HeadSHA: "aaa", Status: store.Running})
	_ = st.SetPlan(id, "t", "a", "aaa", "medium", "sonnet → opus", "90 lines", 90, 4, 2)
	_ = st.AddCost(id, 2.25)
	_ = st.Finish(id, store.Done, "you approved", "", "")

	cfg := config.Config{Org: "acme", Threshold: 80, AutoPost: []string{"low"}, Watermark: "🤖 bot",
		ApproveText: "LGTM :+1:", PollInterval: 2 * time.Minute, MaxParallel: 2, MaxAge: 14 * 24 * time.Hour}
	s := &server{d: pipeline.Deps{Store: st, Cfg: cfg}, login: "octocat",
		accounts: &accountCache{at: time.Now(), acc: gh.Account{Login: "octocat", Name: "Mona",
			HTMLURL: "https://github.com/octocat", Orgs: []string{"acme", "umbrella"}}}}

	rec := httptest.NewRecorder()
	s.profile(rec, httptest.NewRequest(http.MethodGet, "/profile", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	for _, want := range []string{"Acting as", "Mona", "@octocat", "acme", "umbrella",
		`class="on"`,                                         // the org it watches is marked among the ones you belong to
		"never through a team",                               // how it decides whose PRs to review
		"80%+ certainty, of low severity",                    // what goes out without asking
		"high severity, design concerns, and every approval", // what always waits
		"14 days", "$2.25"} {
		if !strings.Contains(page, want) {
			t.Errorf("profile page is missing %q", want)
		}
	}
	if strings.Contains(page, "avatars.githubusercontent.com") {
		t.Error("the page points at GitHub for the picture; it should serve /avatar itself")
	}
}

// Without GitHub the page still says what the bot is configured to do, and names the account from start-up.
func TestProfileWithoutGitHub(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{d: pipeline.Deps{Store: st, Cfg: config.Config{Threshold: 80}}, login: "octocat",
		accounts: &accountCache{at: time.Now(), err: errors.New("no network")}}
	rec := httptest.NewRecorder()
	s.profile(rec, httptest.NewRequest(http.MethodGet, "/profile", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, want := range []string{"no network", "@octocat", "every finding waits for you"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("offline profile page is missing %q", want)
		}
	}
}
