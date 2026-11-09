package web

import (
	"encoding/json"
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
	"github.com/badam1/personal-review-bot/internal/update"
)

// site serves the pages through the guard, the way the browser reaches them.
type site struct {
	t   *testing.T
	st  *store.Store
	h   http.Handler
	srv *server
}

func newSite(t *testing.T) *site {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &server{d: pipeline.Deps{Store: st, Log: t.Logf, Cfg: config.Config{Threshold: 80, AutoPost: []string{"low", "medium"},
		ConfigDir: t.TempDir()}}, accounts: &accountCache{at: time.Now(), err: errors.New("GitHub isn't asked in tests")},
		token: "secret", hosts: []string{"localhost:12346"}, origins: []string{"http://localhost:12346"},
		fetch:   func(string, int) (gh.PR, error) { return gh.PR{HeadSHA: "aaa"}, nil },
		threads: func(string, int) ([]gh.Thread, error) { return nil, nil }, login: "octocat",
		send: func(*store.Review, string, []store.Finding, []pipeline.Reminder) (string, error) {
			t.Fatal("nothing may be sent to GitHub unless the test says so")
			return "", nil
		}}
	header = s.headerView
	defaultColumn := labelsColumn
	labelsColumn = func() string { return s.cfg().LabelsColumn() }
	t.Cleanup(func() { header, labelsColumn = nil, defaultColumn })
	return &site{t: t, st: st, h: s.guard(s.routes()), srv: s}
}

func (s *site) get(path string) (int, string) {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346"+path, nil))
	return rec.Code, rec.Body.String()
}

func (s *site) post(path string, form url.Values) *httptest.ResponseRecorder {
	form.Set("csrf", "secret")
	req := httptest.NewRequest(http.MethodPost, "http://localhost:12346"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// review adds a finished review of o/r#number with the given status.
func (s *site) review(number int, status, decision string) int64 {
	id, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: number, Title: fmt.Sprintf("PR %d", number),
		Author: "dev", HeadSHA: "aaa", Status: store.Running})
	_ = s.st.SetPlan(id, fmt.Sprintf("PR %d", number), "dev", "aaa", "medium", "sonnet → opus",
		"risky paths: prod/iam.tf · 120 lines in 2 files", 100, 20, 2)
	if status != store.Running {
		_ = s.st.Finish(id, status, decision, "", "")
	}
	return id
}

func want(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(page, w) {
			t.Errorf("page is missing %q", w)
		}
	}
}

// The main page counts what waits in the tab title, refreshes itself, shows what runs, and links each PR's timeline.
func TestIndexLive(t *testing.T) {
	s := newSite(t)
	s.review(1, store.AwaitingYou, "2 need you")
	s.review(2, store.AwaitingYou, "no issues: approve?")
	s.review(3, store.Running, "")
	_, page := s.get("/")
	want(t, page, `<a class="brand" href="/"><svg`, "</svg>personal-review-bot</a>", "<title>(2) personal-review-bot</title>",
		`<meta http-equiv="refresh" content="60; url=?">`, `<h2 id="running-h" class="h2">Running</h2>`,
		`data-running-pill data-status="running"`, `<span class="count-n">1</span> running`,
		`href="/pr/acme/infra/3"`, ">infra#3<", `<a href="/history">All reviews</a>`, `<span class="count need">2</span>`)
	if strings.Contains(page, ">acme/infra#") {
		t.Error("lists should show the repo without its owner")
	}
}

// A finding shows the code around its line, links the line on GitHub, and the review says why it got its tier.
func TestReviewCode(t *testing.T) {
	s := newSite(t)
	id := s.review(4, store.AwaitingYou, "1 need you")
	fid, _ := s.st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "medium", Path: "dir/a b.go", Line: 11,
		Title: "Nil map", Comment: "c", Status: store.Pending})
	_ = s.st.SetContext(fid, "10\t \tfunc f() {\n11\t+\t\tm[k] = v\n12\t \t}")
	_, page := s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, `href="https://github.com/acme/infra/blob/aaa/dir/a%20b.go#L11"`,
		`<span class="add tgt"><i>11</i><b>+</b>	m[k] = v</span>`, `<span class=""><i>10</i>`,
		`<dt>Why</dt><dd>risky paths: prod/iam.tf · 120 lines in 2 files</dd>`, `href="/pr/acme/infra/4"`)

	running := s.review(5, store.Running, "")
	_, page = s.get(fmt.Sprintf("/r/%d", running))
	want(t, page, `<meta http-equiv="refresh" content="15; url=?">`)

	// A review that still needs you, and its PR's timeline, sit under Reviews.
	_, page = s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, `<a href="/" aria-current="page">Reviews</a>`, `<nav class="crumbs" aria-label="Breadcrumb"><a href="/">Reviews</a>`)
	_, page = s.get("/pr/acme/infra/4")
	want(t, page, `<a href="/" aria-current="page">Reviews</a>`, `<span class="mono" aria-current="page">acme/infra#4</span>`)
}

// A posted finding can be marked wrong and unmarked; one that wasn't posted can't.
func TestMarkWrong(t *testing.T) {
	s := newSite(t)
	id := s.review(6, store.Done, "posted 1")
	posted, _ := s.st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "high", Path: "a.go", Line: 1,
		Title: "Off by one", Comment: "c", Status: store.Candidate})
	_ = s.st.UpdateFinding(&store.Finding{ID: posted, Line: 1, Comment: "c", Confidence: 92, Status: store.AutoPosted})
	dropped, _ := s.st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "low", Path: "a.go", Line: 2,
		Title: "Nit", Comment: "c", Status: store.Candidate})
	_ = s.st.UpdateFinding(&store.Finding{ID: dropped, Line: 2, Comment: "c", Confidence: 55, Status: store.Discarded})

	_, page := s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, fmt.Sprintf(`action="/f/%d/wrong"`, posted), "Mark as wrong", `<span><b class="cert hi">92%</b> · logic</span>`,
		`<span><b class="cert lo">55%</b> · logic</span>`, `<div class="acts">`)
	if strings.Contains(page, fmt.Sprintf(`action="/f/%d/wrong"`, dropped)) {
		t.Error("a dropped finding can't be marked wrong")
	}

	back := fmt.Sprintf("/r/%d", id)
	if rec := s.post(fmt.Sprintf("/f/%d/wrong", posted), url.Values{"wrong": {"1"}, "back": {back}}); rec.Header().Get("Location") != back {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	if f, _ := s.st.Finding(posted); !f.Wrong {
		t.Fatal("not marked wrong")
	}
	_, page = s.get(back)
	want(t, page, "marked wrong", "Undo")
	_, stats := s.get("/stats")
	want(t, stats, "1 marked wrong", "(0% right)")

	if rec := s.post(fmt.Sprintf("/f/%d/wrong", dropped), url.Values{"wrong": {"1"}, "back": {back}}); !strings.Contains(rec.Header().Get("Location"), "msg=") {
		t.Error("marking a dropped finding should come back with a message")
	}
	if rec := s.post(fmt.Sprintf("/f/%d/wrong", posted), url.Values{"wrong": {"0"}, "back": {"https://evil.example"}}); rec.Header().Get("Location") != "/" {
		t.Errorf("an outside back address must not be followed: %q", rec.Header().Get("Location"))
	}
	if f, _ := s.st.Finding(posted); f.Wrong {
		t.Error("undo did not take the mark back")
	}
}

// History filters and pages through every review, and lists ignored PRs with a way back.
func TestHistory(t *testing.T) {
	s := newSite(t)
	for n := 1; n <= historyPerPage+5; n++ {
		s.review(n, store.Done, "you approved")
	}
	s.review(100, store.Failed, "failed")
	_ = s.st.IgnorePR("acme/infra", 100)

	_, page := s.get("/history")
	want(t, page, fmt.Sprintf("%d review(s)", historyPerPage+6), "page 1 of 2", `href="/history?page=2"`, "Older →",
		"Ignored PRs", "PR 100", `action="/unignore"`)
	_, page = s.get("/history?status=failed")
	want(t, page, "1 review(s)", `<option value="failed" selected>`)
	if strings.Contains(page, ">PR 7<") {
		t.Error("the status filter let a done review through")
	}

	rec := s.post("/unignore", url.Values{"repo": {"acme/infra"}, "number": {"100"}, "back": {"/history"}})
	if rec.Code != http.StatusSeeOther || s.st.IsIgnored("acme/infra", 100) {
		t.Errorf("un-ignore: %d, still ignored %v", rec.Code, s.st.IsIgnored("acme/infra", 100))
	}
}

// The timeline shows every review of a PR with what started it and what became of each finding.
func TestTimeline(t *testing.T) {
	s := newSite(t)
	first := s.review(9, store.Done, "you posted 1")
	f, _ := s.st.AddFinding(&store.Finding{ReviewID: first, Agent: "security", Severity: "high", Path: "iam.tf", Line: 3,
		Title: "Wildcard action", Comment: "c", Status: store.Candidate})
	_ = s.st.UpdateFinding(&store.Finding{ID: f, Line: 3, Comment: "c", Confidence: 88, Status: store.Posted})
	_ = s.st.SetWrong(f, true)
	s.review(9, store.AwaitingYou, "no issues: approve?")
	superseded := s.review(9, store.Done, "superseded by review 99")
	_, _ = s.st.AddFinding(&store.Finding{ReviewID: superseded, Agent: "logic", Severity: "low", Title: "Left behind",
		Comment: "c", Status: store.Pending})

	code, page := s.get("/pr/acme/infra/9")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want(t, page, "3 reviews", "Wildcard action", "you posted · marked wrong", `security · <span class="where">iam.tf:3</span>`,
		"<b>Needs you</b>", "never decided")
	// Once its latest review is over, the PR and its finished reviews sit under History, wherever you came from.
	want(t, page, `<a href="/history" aria-current="page">History</a>`, `<nav class="crumbs" aria-label="Breadcrumb"><a href="/history">History</a>`)
	_, page = s.get(fmt.Sprintf("/r/%d", first))
	want(t, page, `<a href="/history" aria-current="page">History</a>`, `<a href="/history">History</a> <span aria-hidden="true">/</span> <a class="mono" href="/pr/acme/infra/9"`,
		`<span aria-current="page">`)
	if code, _ := s.get("/pr/acme/infra/404"); code != http.StatusNotFound {
		t.Errorf("unknown PR: %d", code)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{"/r/3": "/r/3", "//evil.example": "/", `/\evil`: "/", "https://x": "/", "": "/"} {
		if got := localPath(in, "/"); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A running review gets a live line: spinner, verb, clock and stage, driven by running.js from /active.json.
func TestRunningLine(t *testing.T) {
	s := newSite(t)
	id := s.review(7, store.Running, "")
	_ = s.st.SetStage(id, "verifying 3 finding(s) (opus)")
	queued, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: 8, Title: "PR 8", Status: store.Queued})

	_, page := s.get("/")
	want(t, page, "data-running-all", fmt.Sprintf(`class="running" data-id="%d" data-status="running"`, id),
		`<span class="verb">Thinking…</span>`, `<span class="stage">verifying 3 finding(s) (opus)</span>`,
		fmt.Sprintf(`data-id="%d" data-status="queued" data-started="0"`, queued), "Waiting in line…",
		`<script src="/running.js" defer></script>`)
	_, page = s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, fmt.Sprintf(`class="running" data-id="%d"`, id), `<script src="/running.js" defer></script>`)
	if strings.Contains(page, "data-running-all") {
		t.Error("a review page watches only its own review")
	}

	code, body := s.get("/active.json")
	var active []struct {
		ID      int64
		Status  string
		Stage   string
		Started int64
	}
	if err := json.Unmarshal([]byte(body), &active); code != http.StatusOK || err != nil || len(active) != 2 {
		t.Fatalf("active.json: %d %v %s", code, err, body)
	}
	if a := active[0]; a.ID != id || a.Status != "running" || a.Stage != "verifying 3 finding(s) (opus)" || a.Started == 0 {
		t.Errorf("running = %+v", a)
	}
	if a := active[1]; a.Status != "queued" || a.Started != 0 {
		t.Errorf("queued = %+v", a)
	}

	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346/running.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `fetch("/active.json"`) {
		t.Errorf("running.js: %d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self';") ||
		!strings.Contains(csp, "script-src 'self';") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestSince(t *testing.T) {
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 7*time.Minute + 45*time.Second: "7m 45s",
		62*time.Minute + 5*time.Second: "1h 2m"} {
		if got := since(time.Now().Add(-d)); got != want {
			t.Errorf("since(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestOrdinal(t *testing.T) {
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th",
		21: "21st", 22: "22nd", 101: "101st", 111: "111th"} {
		if got := Ordinal(n); got != want {
			t.Errorf("Ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// A re-review says which iteration it is and what started it; the first review says nothing, the timeline numbers all.
func TestIterationLabel(t *testing.T) {
	s := newSite(t)
	first := s.review(12, store.Done, "you approved")
	again, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: 12, Title: "PR 12", Author: "dev",
		HeadSHA: "bbb", Status: store.Running, Source: "new commits"})
	_ = s.st.SetPlan(again, "PR 12", "dev", "bbb", "medium", "sonnet → opus", "", 100, 20, 2)
	_ = s.st.Finish(again, store.AwaitingYou, "1 need you", "", "")

	_, page := s.get(fmt.Sprintf("/r/%d", again))
	want(t, page, `<span>2nd iteration · new commits</span>`)
	_, page = s.get(fmt.Sprintf("/r/%d", first))
	if strings.Contains(page, "iteration") {
		t.Error("a first review carries no iteration label")
	}
	_, page = s.get("/")
	want(t, page, "· @dev · 2nd iteration · new commits ·", `<span>2nd iteration · new commits</span>`)
	_, page = s.get("/pr/acme/infra/12")
	want(t, page, "<span>1st iteration</span>", "<span>2nd iteration</span>")
}

// A re-review lists what became of the earlier findings, and approving with a high one still open asks once more.
func TestEarlierFindings(t *testing.T) {
	s := newSite(t)
	first := s.review(20, store.Done, "you posted 2")
	add := func(title, severity, followup, note string) {
		id, _ := s.st.AddFinding(&store.Finding{ReviewID: first, Agent: "logic", Severity: severity, Path: "a.go", Line: 3,
			Title: title, Comment: "c", Status: store.Candidate})
		_ = s.st.UpdateFinding(&store.Finding{ID: id, Line: 3, Comment: "c", Status: store.Posted})
		if followup != "" {
			_ = s.st.SetFollowup(id, followup, note, first+1)
		}
	}
	add("Nil map write", "high", store.FollowFixed, "Fixed in abc123: the map is made in New.")
	add("Unchecked error", "high", store.FollowOpen, "Still ignored at a.go:42.")
	add("Typo", "low", "", "")
	again, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: 20, Title: "PR 20", HeadSHA: "aaa",
		Status: store.Running, Source: "new commits", DryRun: true})
	_ = s.st.SetPlan(again, "PR 20", "dev", "aaa", "medium", "sonnet → opus", "", 100, 20, 2)
	_ = s.st.Finish(again, store.AwaitingYou, "no issues: approve?, 1 earlier still open", "", "")

	_, page := s.get(fmt.Sprintf("/r/%d", again))
	want(t, page, `<span class="count ok">1 fixed</span><span class="count open">1 still open</span><span class="count skip">1 not checked yet</span>`,
		"fixed · Fixed in abc123: the map is made in New.", "still open · Still ignored at a.go:42.", `<p class="sub">not checked yet</p>`,
		"1 high-severity finding(s) posted earlier are still open")

	back := fmt.Sprintf("/r/%d", again)
	rec := s.post(back, url.Values{"action": {"approve"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "confirm=1") {
		t.Fatalf("the first approve should ask: %q", loc)
	}
	if r, _ := s.st.Get(again); r.Status != store.AwaitingYou {
		t.Fatal("approved without asking")
	}
	_, page = s.get(back + "?confirm=1")
	want(t, page, `<input type="hidden" name="anyway" value="1">`)
	s.post(back, url.Values{"action": {"approve"}, "anyway": {"1"}})
	if r, _ := s.st.Get(again); r.Status != store.Done || r.Decision != "dry run: approved by you (nothing sent to GitHub)" {
		t.Errorf("the second approve should go through, status %s, decision %q", r.Status, r.Decision)
	}
	_, page = s.get("/pr/acme/infra/20")
	want(t, page, "you posted · fixed", "you posted · still open")
}

// Failed reviews show on the main page with a retry; a retry queues the PR again the way the loop first meant to,
// never twice, and "Retry all" picks up every one the page lists.
func TestRetry(t *testing.T) {
	s := newSite(t)
	fail := func(number int, source string) int64 {
		id, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: number, Title: fmt.Sprintf("PR %d", number),
			Author: "dev", HeadSHA: "aaa", Status: store.Running, Source: source})
		_ = s.st.Finish(id, store.Failed, "failed", "", "fetch PR: HTTP 502")
		return id
	}
	one := fail(1, "re-request")
	fail(2, "auto")
	fail(3, "new commits")

	_, page := s.get("/")
	want(t, page, "Failed · last 7 days", "Retry all 3", fmt.Sprintf(`name="id" value="%d"`, one), "fetch PR: HTTP 502")
	_, page = s.get(fmt.Sprintf("/r/%d", one))
	want(t, page, `value="rereview">`, `</svg>Retry</button>`)

	rec := s.post("/retry", url.Values{"id": {fmt.Sprint(one)}})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("retry: %d to %q, want a redirect to /", rec.Code, loc)
	}
	latest, _ := s.st.Latest("acme/infra", 1)
	if latest.ID == one || latest.Status != store.Queued || latest.Source != "re-request" {
		t.Fatalf("retry should queue a new re-request review, latest is %d (%s, %s)", latest.ID, latest.Status, latest.Source)
	}
	s.post("/retry", url.Values{"id": {fmt.Sprint(one)}})
	if again, _ := s.st.Latest("acme/infra", 1); again.ID != latest.ID {
		t.Error("a second retry while one is queued should not queue another")
	}

	_, page = s.get("/")
	want(t, page, "Failed · last 7 days")
	if strings.Contains(page, fmt.Sprintf(`name="id" value="%d"`, one)) {
		t.Error("a retried PR should leave the failed list")
	}
	s.post("/retry", url.Values{})
	for number, source := range map[int]string{2: "auto", 3: "new commits"} {
		if r, _ := s.st.Latest("acme/infra", number); r.Status != store.Queued || r.Source != source {
			t.Errorf("retry all: #%d is %s (%s), want queued (%s)", number, r.Status, r.Source, source)
		}
	}
	if _, page = s.get("/"); strings.Contains(page, "Failed · last 7 days") {
		t.Error("nothing failed is left, the section should go")
	}
}

// Every page links the icon as its favicon, both files are served, and the CSP lets the browser load them.
func TestFavicon(t *testing.T) {
	s := newSite(t)
	for _, path := range []string{"/", "/history", "/stats"} {
		_, page := s.get(path)
		want(t, page, `<link rel="icon" href="/favicon.png" sizes="32x32" type="image/png">`,
			`<link rel="icon" href="/icon.svg" type="image/svg+xml">`)
	}
	for path, typ := range map[string]string{"/icon.svg": "image/svg+xml", "/favicon.png": "image/png"} {
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346"+path, nil))
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), typ) || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %q, %d bytes", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self'") {
			t.Errorf("CSP doesn't allow the icon: %q", csp)
		}
	}
}

// The main page offers a newer release with a button (only when the daemon serves it), shows an update under way
// instead of the button, and offers it again after a failure.
func TestUpdateNotice(t *testing.T) {
	s := newSite(t)
	s.srv.version = "v0.1.0-beta.2"
	if _, page := s.get("/"); strings.Contains(page, "is available") {
		t.Error("no check yet, nothing to offer")
	}
	rel := update.Release{Version: "v0.1.0-beta.3", URL: "https://github.com/o/r/releases/tag/v0.1.0-beta.3"}
	update.Save(s.st, update.Check{Checked: time.Now(), Latest: &rel})

	_, page := s.get("/")
	want(t, page, `<span class="dot new"></span>v0.1.0-beta.3 available`, `<div class="ttl new">v0.1.0-beta.3 is available</div>`,
		"You run v0.1.0-beta.2</div>", `href="https://github.com/o/r/releases/tag/v0.1.0-beta.3"`)
	if strings.Contains(page, `action="/update"`) {
		t.Error("the page alone can't install, so it offers no button")
	}

	fake := &fakeUpdates{start: "Updating to v0.1.0-beta.3: it restarts once the running reviews finish."}
	s.srv.updates = fake
	_, page = s.get("/")
	want(t, page, `action="/update"`, "Update and restart")
	rec := s.post("/update", url.Values{})
	if loc := rec.Header().Get("Location"); fake.starts != 1 || !strings.HasPrefix(loc, "/?msg=Updating+to+v0.1.0-beta.3") {
		t.Fatalf("POST /update: %d calls, redirect %q", fake.starts, loc)
	}
	_, page = s.get(rec.Header().Get("Location"))
	want(t, page, "Updating to v0.1.0-beta.3: it restarts once the running reviews finish.")

	update.SetState(s.st, "v0.1.0-beta.3 installed: restarting once 1 running review finishes")
	_, page = s.get("/")
	want(t, page, `id="vmenu" data-version="v0.1.0-beta.2" data-busy`, `<b class="verb">Updating to v0.1.0-beta.3…</b>`,
		`<li class="now">`, `<span class="label">Restarting once 1 running review finishes</span>`, `<script src="/update.js" defer>`)
	if strings.Contains(page, `action="/update"`) {
		t.Error("no second update while one is under way")
	}
	update.SetState(s.st, "updating to v0.1.0-beta.3 failed: attestation not verified")
	_, page = s.get("/")
	want(t, page, "failed: attestation not verified", `action="/update"`)

	s.srv.updates = nil
	if rec := s.post("/update", url.Values{}); rec.Code != http.StatusNotFound {
		t.Errorf("the page alone: POST /update = %d", rec.Code)
	}
}

// Every page ends with the footer: the version linked to its release, the platform, help links into the repo (the bug
// form filled in with both), and where your files are.
func TestPageFooter(t *testing.T) {
	defer func(a About) { about = a }(about)
	about = About{Version: "v0.1.0-beta.4", Platform: "darwin/arm64", Repo: "https://github.com/acme/prb",
		ConfigDir: "~/.config/personal-review-bot", StateDir: "~/.local/state/personal-review-bot", Log: "~/Library/Logs/personal-review-bot.log"}
	s := newSite(t)
	id := s.review(1, store.Done, "you approved")
	for _, path := range []string{"/", "/history", "/stats", "/settings", fmt.Sprintf("/r/%d", id), "/pr/acme/infra/1"} {
		_, page := s.get(path)
		want(t, page, `<footer class="foot">`,
			`<a href="https://github.com/acme/prb/releases/tag/v0.1.0-beta.4" target="_blank" rel="noopener" title="Release notes">v0.1.0-beta.4</a> · darwin/arm64`,
			`href="https://github.com/acme/prb/issues/new?platform=darwin%2Farm64&amp;template=bug_report.yml&amp;version=v0.1.0-beta.4"`,
			`href="https://github.com/acme/prb/issues/new?template=feature_request.yml"`, `href="https://github.com/acme/prb#readme"`,
			`<a href="/settings">Settings</a>`, `href="https://github.com/acme/prb" target="_blank" rel="noopener">Source</a>`,
			`<script src="/bar.js" defer></script>`,
			`title="config ~/.config/personal-review-bot · data ~/.local/state/personal-review-bot · log ~/Library/Logs/personal-review-bot.log"`)
	}

	about.Version = "v0.1.0-beta.4-2-gabc1234"
	if got := about.ReleaseURL(); got != "https://github.com/acme/prb/releases" {
		t.Errorf("a build between releases links the releases list, got %q", got)
	}
}

type fakeUpdates struct {
	start, check   string
	starts, checks int
}

func (f *fakeUpdates) Start() (string, error)    { f.starts++; return f.start, nil }
func (f *fakeUpdates) CheckNow() (string, error) { f.checks++; return f.check, nil }

// Check for updates asks the daemon to look right away and says what it found; the page alone has no such link.
func TestCheckForUpdates(t *testing.T) {
	s := newSite(t)
	if _, page := s.get("/"); strings.Contains(page, "Check for updates") {
		t.Error("the page alone can't check")
	}
	fake := &fakeUpdates{check: "v0.1.0-beta.5 is available."}
	s.srv.updates = fake
	_, page := s.get("/")
	want(t, page, `action="/update/check"`, `<button class="btn quiet wide">Check for updates</button>`)
	rec := s.post("/update/check", url.Values{})
	if loc := rec.Header().Get("Location"); fake.checks != 1 || loc != "/?msg=v0.1.0-beta.5+is+available." {
		t.Errorf("POST /update/check: %d checks, redirect %q", fake.checks, loc)
	}
	if fake.starts != 0 {
		t.Error("checking must not install")
	}
}

// Still-open earlier findings can be reminded of: unanswered ones start ticked, a reply or a reminder at this commit
// unticks one, a resolved thread is reopened, and the ticked ones go out with the review you post.
func TestReminders(t *testing.T) {
	s := newSite(t)
	s.srv.d.Cfg.Watermark = "🤖 acme-bot"
	first := s.review(30, store.Done, "bot posted 4")
	add := func(title, agent, path string, inline bool, followup, note string) int64 {
		id, _ := s.st.AddFinding(&store.Finding{ReviewID: first, Agent: agent, Severity: "high", Path: path, Line: 7,
			Title: title, Comment: "The " + title + " is wrong.", Status: store.Candidate})
		_ = s.st.UpdateFinding(&store.Finding{ID: id, Line: 7, Comment: "The " + title + " is wrong.", Status: store.Posted,
			Inline: inline, PostedURL: "https://github.com/acme/infra/pull/30#pullrequestreview-1"})
		_ = s.st.SetFollowup(id, followup, note, first)
		return id
	}
	quiet := add("retry loop", "logic", "a.go", true, store.FollowOpen, "a.go:9 still returns nil.")
	answered := add("timeout", "logic", "b.go", true, store.FollowOpen, "b.go:7 unchanged.")
	resolved := add("port", "integration", "c.go", true, store.FollowPartly, "c.go:7 fixed, d.go:3 not.")
	design := add("second release path", "design", "", false, store.FollowOpen, "")
	reminded := add("log level", "logic", "e.go", true, store.FollowOpen, "e.go:7 unchanged.")
	_ = add("nil map", "logic", "f.go", true, store.FollowFixed, "Fixed in bbb.")
	_ = s.st.SetReminded(reminded, "aaa")

	thread := func(id, path, finding string, resolved bool, replies ...gh.ThreadComment) gh.Thread {
		first := gh.ThreadComment{Author: "octocat", Body: "The " + finding + " is wrong.\n\n<sub>🤖 acme-bot · 91%</sub>"}
		return gh.Thread{ID: id, Path: path, Resolved: resolved, Comments: append([]gh.ThreadComment{first}, replies...)}
	}
	s.srv.threads = func(string, int) ([]gh.Thread, error) {
		return []gh.Thread{
			thread("T1", "a.go", "retry loop", false),
			thread("T2", "b.go", "timeout", false, gh.ThreadComment{Author: "dev", Body: "Doing it in a follow-up PR."}),
			thread("T3", "c.go", "port", true),
			thread("T5", "e.go", "log level", false),
			{ID: "T9", Path: "a.go", Comments: []gh.ThreadComment{{Author: "dev", Body: "The retry loop is wrong."}}},
		}, nil
	}
	again := s.review(30, store.AwaitingYou, "no issues: approve?, 4 earlier still open")

	_, page := s.get(fmt.Sprintf("/r/%d", again))
	ticked := func(id int64) bool { return strings.Contains(page, fmt.Sprintf(`name="rm_%d" checked`, id)) }
	if !ticked(quiet) || ticked(answered) || !ticked(resolved) || !ticked(design) || ticked(reminded) {
		t.Errorf("ticked: quiet %v, answered %v, resolved %v, design %v, reminded %v (want true false true true false)",
			ticked(quiet), ticked(answered), ticked(resolved), ticked(design), ticked(reminded))
	}
	want(t, page, `<form id="decide"`, "Still open at `aaa`: a.go:9 still returns nil.", "Only partly fixed at `aaa`:",
		"Last reply: @dev: Doing it in a follow-up PR.", "resolved: reminding reopens it", "in the review body",
		"you reminded the author at aaa", "Nothing new found, but 5 earlier finding(s) are still open", `value="post"`,
		`</svg>View comment<svg class="ext"`, `title="The review that checked this finding">Later review<svg`)
	if strings.Contains(page, fmt.Sprintf(`name="rm_%d"`, quiet+5)) {
		t.Error("a fixed finding can't be reminded of")
	}

	var sent []pipeline.Reminder
	s.srv.send = func(_ *store.Review, event string, fs []store.Finding, rms []pipeline.Reminder) (string, error) {
		if event != "COMMENT" || len(fs) != 0 {
			t.Errorf("sent %s with %d findings", event, len(fs))
		}
		sent = rms
		return "https://github.com/acme/infra/pull/30#pullrequestreview-2", nil
	}
	back := fmt.Sprintf("/r/%d", again)
	s.post(back, url.Values{"action": {"post"}, fmt.Sprintf("rm_%d", quiet): {"on"}, fmt.Sprintf("rt_%d", quiet): {"Please fix the retry."},
		fmt.Sprintf("rm_%d", resolved): {"on"}, fmt.Sprintf("rm_%d", design): {"on"}, fmt.Sprintf("rt_%d", design): {""}})
	if len(sent) != 3 {
		t.Fatalf("sent %d reminders, want 3", len(sent))
	}
	byID := map[int64]pipeline.Reminder{}
	for _, rm := range sent {
		byID[rm.Finding.ID] = rm
	}
	if rm := byID[quiet]; rm.Text != "Please fix the retry." || rm.Thread == nil || rm.Thread.ID != "T1" {
		t.Errorf("quiet: %+v", rm)
	}
	if rm := byID[resolved]; rm.Thread == nil || rm.Thread.ID != "T3" || !rm.Thread.Resolved {
		t.Errorf("resolved: %+v", rm)
	}
	if rm := byID[design]; rm.Thread != nil || rm.Text != "Still open at `aaa`." {
		t.Errorf("design: %+v (an emptied text falls back to the default)", rm)
	}
	r, _ := s.st.Get(again)
	if r.Status != store.Done || r.Decision != "you reminded of 3 still open" {
		t.Errorf("review: %s, %q", r.Status, r.Decision)
	}
	earlier, _ := s.st.EarlierPosted("acme/infra", 30, again)
	for _, f := range earlier {
		if want := f.ID == quiet || f.ID == resolved || f.ID == design || f.ID == reminded; (f.RemindedSHA == "aaa") != want {
			t.Errorf("finding %d (%s) reminded at %q", f.ID, f.Title, f.RemindedSHA)
		}
	}
}

// Posting needs a finding or a reminder ticked.
func TestPostNothing(t *testing.T) {
	s := newSite(t)
	id := s.review(31, store.AwaitingYou, "no issues: approve?")
	rec := s.post(fmt.Sprintf("/r/%d", id), url.Values{"action": {"post"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "finding+or+reminder") {
		t.Errorf("redirect = %q", loc)
	}
}

// Deep review queues the PR again as a deep review, from a review waiting for you or a finished one; a retry of a
// failed deep review stays deep.
func TestDeepReview(t *testing.T) {
	s := newSite(t)
	waiting := s.review(40, store.AwaitingYou, "2 need you")
	_, page := s.get(fmt.Sprintf("/r/%d", waiting))
	want(t, page, `name="action" value="deep"`, ">Deep review</button>")
	rec := s.post(fmt.Sprintf("/r/%d", waiting), url.Values{"action": {"deep"}})
	latest, _ := s.st.Latest("acme/infra", 40)
	if latest.ID == waiting || !latest.Deep || latest.Source != "manual" || latest.Status != store.Queued {
		t.Fatalf("deep review queued %+v", latest)
	}
	if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/r/%d", latest.ID) {
		t.Errorf("redirect = %q", loc)
	}
	_, page = s.get(fmt.Sprintf("/r/%d", latest.ID))
	want(t, page, `<span class="tag deep">deep review</span>`)

	done := s.review(41, store.Done, "you approved")
	_, page = s.get(fmt.Sprintf("/r/%d", done))
	want(t, page, `name="action" value="deep"`, `title="Review the PR again at its latest commit">`, `</svg>Re-review</button>`)
	s.post(fmt.Sprintf("/r/%d", done), url.Values{"action": {"rereview"}})
	if r, _ := s.st.Latest("acme/infra", 41); r.Deep {
		t.Error("a plain re-review must not be deep")
	}

	failed, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: 42, Status: store.Running, Source: "manual", Deep: true})
	_ = s.st.Finish(failed, store.Failed, "failed", "", "fetch PR: 502")
	s.post(fmt.Sprintf("/r/%d", failed), url.Values{"action": {"rereview"}})
	if r, _ := s.st.Latest("acme/infra", 42); r.ID == failed || !r.Deep {
		t.Errorf("retrying a failed deep review: %+v", r)
	}
}

// While an update runs, the click comes back to a clean main page, whose notice follows /update.json; once the daemon
// is back on the new version the page says so for an hour.
func TestUpdateProgress(t *testing.T) {
	s := newSite(t)
	s.srv.version = "v0.1.0"
	busy := &fakeUpdates{start: "Updating to v0.1.1."}
	s.srv.updates = busy
	update.SetState(s.st, "downloading v0.1.1 (6.2 MB)")
	if loc := s.post("/update", url.Values{}).Header().Get("Location"); loc != "/" {
		t.Errorf("a started update should come back to a clean page, got %q", loc)
	}
	_, js := s.get("/update.json")
	want(t, js, `"version":"v0.1.0"`, `"state":"downloading v0.1.1 (6.2 MB)"`, `"busy":true`)

	// The daemon restarts into v0.1.1.
	update.Restarting(s.st, "v0.1.0")
	if d, ok := update.Started(s.st, "v0.1.1"); !ok || d.From != "v0.1.0" || d.To != "v0.1.1" {
		t.Fatalf("Started = %+v, %v", d, ok)
	}
	s.srv.version = "v0.1.1"
	_, page := s.get("/")
	want(t, page, "<span>Updated to v0.1.1</span>", "from v0.1.0 ·", "What's new in v0.1.1",
		`href="https://github.com/badam1/personal-review-bot/releases/tag/v0.1.1"`)
	if strings.Contains(page, "data-busy>") {
		t.Error("the update is over")
	}
	_, js = s.get("/update.json")
	want(t, js, `"version":"v0.1.1"`, `"busy":false`)
	if _, again := update.Started(s.st, "v0.1.1"); again {
		t.Error("an ordinary restart isn't an update")
	}
}

// A review opens with what the PR is about, then what the bots found, the findings, and how it was reviewed: the design
// review and the verifier first. The reviewer filter shows once more than one reviewer found something.
func TestReviewOverview(t *testing.T) {
	s := newSite(t)
	id := s.review(50, store.AwaitingYou, "2 need you")
	_ = s.st.SetAbout(id, "Retries a capture that times out, up to `3` times. Captures time out now and then.")
	_ = s.st.SetApproval(id, 64, "The retry loop was traced, but nobody read `client.go`.")
	add := func(agent, severity string, confidence int, verdict, status string) {
		fid, _ := s.st.AddFinding(&store.Finding{ReviewID: id, Agent: agent, Severity: severity, Path: "a.go", Line: 1,
			Title: agent + " finding", Comment: "c", Status: store.Candidate})
		_ = s.st.UpdateFinding(&store.Finding{ID: fid, Line: 1, Comment: "c", Confidence: confidence, Verdict: verdict, Status: status})
	}
	add("logic", "high", 91, "confirmed", store.Pending)
	add("security", "low", 85, "confirmed", store.AutoPosted)
	for _, r := range []store.Run{{Agent: "logic", Model: "opus", Findings: 1, Checked: "Traced `Capture`."},
		{Agent: "summary", Model: "haiku"}, {Agent: "verifier", Model: "opus", Findings: 2},
		{Agent: "design", Model: "fable", Checked: "The approach fits."}, {Agent: "security", Model: "sonnet", Findings: 1, Denials: 3}} {
		r.ReviewID = id
		_ = s.st.AddRun(&r)
	}

	_, page := s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, `<h2 id="about" class="lbl">About this PR</h2>`, "up to <code>3</code> times",
		`<h2 id="found" class="h2">What the bots found</h2>`, "2 found → 2 confirmed",
		`title="logic: logic finding">high <b class="cert hi">91%</b></span>`,
		`<div class="filter" id="filter" hidden>`, `data-agent="security" aria-pressed="false">security 1</button>`,
		`<script src="/filter.js" defer></script>`, `data-finding data-agent="logic"`, `data-finding data-agent="security"`,
		`<h2 id="how" class="h2">How it was reviewed</h2>`, "wrote the summary at the top", "Traced <code>Capture</code>.",
		`title="3 tool calls outside its read-only tools`,
		`<b class="cert mid">64%</b>`, "confident to approve", "but nobody read <code>client.go</code>.",
		"Look at it yourself before approving.")
	order := []string{"About this PR", "What the bots found", "Findings to decide", "How it was reviewed"}
	for i := 1; i < len(order); i++ {
		if strings.Index(page, order[i-1]) > strings.Index(page, order[i]) {
			t.Errorf("%q comes after %q", order[i-1], order[i])
		}
	}
	how := page[strings.Index(page, `id="how"`):]
	runs := []string{"<b>design</b>", "<b>verifier</b>", "<b>logic</b>", "<b>summary</b>"}
	for i := 1; i < len(runs); i++ {
		if strings.Index(how, runs[i-1]) > strings.Index(how, runs[i]) {
			t.Errorf("%s is listed after %s", runs[i-1], runs[i])
		}
	}
	if strings.Contains(page, `class="card agents"`) {
		t.Error("the sidebar still lists the reviewers")
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346/filter.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "prb:filter") {
		t.Errorf("filter.js: %d", rec.Code)
	}
}

// The PR's risk labels show beside its size: on the review, in the lists and on its timeline. Opening a review that
// waits for you brings them up to date, since a labeler may add one after the review started.
func TestRiskLabels(t *testing.T) {
	s := newSite(t)
	s.srv.d.Cfg.ShowLabels = []string{"risk/", "auto-risk/"}
	id := s.review(60, store.AwaitingYou, "no issues: approve?")
	_ = s.st.SetLabels(id, []string{"risk/medium"})
	s.srv.fetch = func(string, int) (gh.PR, error) {
		return gh.PR{HeadSHA: "aaa", Labels: []string{"size/m", "risk/medium", "auto-risk/high"}}, nil
	}

	_, page := s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, `<span class="plabel high">auto-risk/high</span><span class="plabel medium">risk/medium</span>`,
		`<dt>Labels</dt>`, `<span class="size medium">medium</span>`)
	if strings.Contains(page, "size/m") {
		t.Error("only the configured labels show")
	}
	if r, _ := s.st.Get(id); strings.Join(r.Labels, ",") != "auto-risk/high,risk/medium" {
		t.Errorf("labels not refreshed: %q", r.Labels)
	}
	for _, path := range []string{"/", "/history", "/pr/acme/infra/60"} {
		_, page := s.get(path)
		want(t, page, `<span class="plabel high">auto-risk/high</span>`)
	}
	_, page = s.get("/history")
	want(t, page, `<th scope="col">Risk</th>`, `<div class="lstack"><span class="plabel high">auto-risk/high</span>`,
		`<span class="ohead"><span class="dot need"></span><b>Needs you</b></span><span class="sub">no issues: approve?</span>`)

	// The column is named after the labels shown, or as LABELS_TITLE says.
	s.srv.d.Cfg.ShowLabels = []string{"size/"}
	_, page = s.get("/history")
	want(t, page, `<th scope="col">Size</th><th scope="col">Size</th>`)
	s.srv.d.Cfg.LabelsTitle = "Priority"
	_, page = s.get("/history")
	want(t, page, `<th scope="col">Priority</th>`)
}

// Every page loads the bar's script, served from this origin like the others.
func TestBarScript(t *testing.T) {
	s := newSite(t)
	_, page := s.get("/history")
	want(t, page, `<script src="/bar.js" defer></script>`)
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346/bar.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `classList.add("stuck")`) {
		t.Errorf("bar.js: %d", rec.Code)
	}
}

// Every button says what it does. A link that leaves for GitHub is an outlined button that names where it goes and
// ends in ↗, and says to screen readers that it opens a new tab; in a table it is a link in words.
func TestLabelledButtons(t *testing.T) {
	s := newSite(t)
	url := "https://github.com/acme/infra/pull/70"
	id, _ := s.st.Insert(&store.Review{Repo: "acme/infra", Number: 70, URL: url, Title: "PR 70", Author: "dev",
		HeadSHA: "aaa", Status: store.Running})
	_ = s.st.Finish(id, store.AwaitingYou, "1 need you", "", "")
	fid, _ := s.st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "low", Path: "a.go", Line: 1,
		Title: "Typo", Comment: "c", Status: store.Candidate})
	_ = s.st.UpdateFinding(&store.Finding{ID: fid, Line: 1, Comment: "c", Confidence: 90, Status: store.AutoPosted,
		PostedURL: url + "#discussion_r1"})

	button := fmt.Sprintf(`<a class="btn out" href="%s" target="_blank" rel="noopener">`, url)
	_, page := s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, `<div class="titlerow"><h1>PR 70</h1>`+button, `Open on GitHub<svg class="ext"`, "(opens in a new tab)",
		`<a class="btn tiny out" href="`+url+`#discussion_r1" target="_blank" rel="noopener">`, `</svg>View comment<svg class="ext"`,
		`</svg>Mark as wrong</button>`)
	for _, gone := range []string{"View on GitHub", "ibtn", "ilink"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %q", gone)
		}
	}
	_, page = s.get("/")
	want(t, page, button, `>Open review</a>`, fmt.Sprintf(`<a class="glink" href="%s" target="_blank" rel="noopener">`, url), `</svg>GitHub<svg class="ext"`)
	_, page = s.get("/pr/acme/infra/70")
	want(t, page, button)
}

// Every control on a review joins the hidden "decide" form, whose first submit button is what Enter in a checkbox
// would press. That must not be a button that posts to GitHub, so the form opens with a disabled one: implicit
// submission does nothing when the default button is disabled.
func TestEnterDoesNotPost(t *testing.T) {
	s := newSite(t)
	id := s.review(4, store.AwaitingYou, "1 need you")
	fid, _ := s.st.AddFinding(&store.Finding{ReviewID: id, Agent: "logic", Severity: "medium", Path: "a.go", Line: 1,
		Title: "Nil map", Comment: "c", Status: store.Pending})
	_ = fid
	_, page := s.get(fmt.Sprintf("/r/%d", id))

	// The checkboxes are there, so Enter has something to submit from.
	want(t, page, `type="checkbox" form="decide"`)

	// Enter presses the first submit button that works, so that button has to be one that does nothing. Disabling
	// it is not enough: Chrome passes over a disabled default button and presses the next one, which is why this
	// insists on the attribute that holds rather than on there being a button at all.
	first := strings.Index(page, `<button type="submit" formmethod="dialog" hidden`)
	if first < 0 {
		t.Fatal("the decide form has no dialog-method default button: Enter in a checkbox would post to GitHub")
	}
	if end := strings.Index(page[first:], ">"); end >= 0 && strings.Contains(page[first:first+end], "disabled") {
		t.Error("the default button is disabled, so Chrome skips it and presses Post selected instead")
	}
	for _, posts := range []string{`value="post"`, `value="approve"`, `value="post_approve"`, `value="rereview"`} {
		if at := strings.Index(page, posts); at >= 0 && at < first {
			t.Errorf("a button with %s comes before the dialog-method default button, so Enter would press it", posts)
		}
	}
}
