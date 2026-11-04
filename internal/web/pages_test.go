package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
)

// site serves the pages through the guard, the way the browser reaches them.
type site struct {
	t  *testing.T
	st *store.Store
	h  http.Handler
}

func newSite(t *testing.T) *site {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &server{d: pipeline.Deps{Store: st, Log: t.Logf, Cfg: config.Config{Threshold: 80, AutoSeverity: "high"}},
		token: "secret", hosts: []string{"localhost:12346"}, origins: []string{"http://localhost:12346"},
		head: func(string, int) (string, error) { return "aaa", nil }}
	return &site{t: t, st: st, h: s.guard(s.routes())}
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
	want(t, page, "<title>(2) personal-review-bot</title>", `<meta http-equiv="refresh" content="60">`, "Running now",
		`href="/pr/acme/infra/3"`, ">infra#3<", `href="/history"`, "All reviews →")
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
		"Why medium: risky paths: prod/iam.tf · 120 lines in 2 files", `href="/pr/acme/infra/4"`)

	running := s.review(5, store.Running, "")
	_, page = s.get(fmt.Sprintf("/r/%d", running))
	want(t, page, `<meta http-equiv="refresh" content="15">`)
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
	_ = s.st.UpdateFinding(&store.Finding{ID: dropped, Line: 2, Comment: "c", Status: store.Discarded})

	_, page := s.get(fmt.Sprintf("/r/%d", id))
	want(t, page, fmt.Sprintf(`action="/f/%d/wrong"`, posted), "Mark as wrong")
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
	want(t, page, "3 review(s)", "Wildcard action", "✅ you posted · marked wrong", "security · iam.tf:3", "🙋 needs you",
		"⏸ never decided")
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
	want(t, page, `<span class="pill">2nd iteration · new commits</span>`)
	_, page = s.get(fmt.Sprintf("/r/%d", first))
	if strings.Contains(page, "iteration") {
		t.Error("a first review carries no iteration label")
	}
	_, page = s.get("/")
	want(t, page, "· 2nd iteration · new commits ·", `<div class="meta">2nd iteration · new commits</div>`)
	_, page = s.get("/pr/acme/infra/12")
	want(t, page, "· 1st iteration ·", "· 2nd iteration ·")
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
	want(t, page, "Earlier findings · 1 fixed · 1 still open · 1 not checked yet", "✅ fixed", "❌ still open",
		"⏳ not checked yet", "Fixed in abc123: the map is made in New.", "1 high-severity finding(s) posted earlier are still open")

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
	if r, _ := s.st.Get(again); r.Status != store.Done {
		t.Errorf("the second approve should go through, status %s", r.Status)
	}
	_, page = s.get("/pr/acme/infra/20")
	want(t, page, "✅ you posted · ✅ fixed", "✅ you posted · ❌ still open")
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
	want(t, page, `value="rereview">Retry</button>`)

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
