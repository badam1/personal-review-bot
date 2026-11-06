//go:build demo

package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
)

// Demo serves the page for a made-up org, acme, until ctx ends: a review waiting with findings and a design concern,
// one with nothing found, one running and a few finished. It is what the README's screenshots show, so nothing in it
// is anyone's real repo, person or account. Built only with -tags demo (go run -tags demo ./cmd/demo); everything it
// writes goes to a temporary directory, and HOME points there so the page's paths read ~/.config/….
func Demo(ctx context.Context, addr, version string) error {
	if err := config.Check("ADDR", addr); err != nil {
		return fmt.Errorf("-addr %s: %w", addr, err)
	}
	home, err := os.MkdirTemp("", "personal-review-bot-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	if err := os.Setenv("HOME", home); err != nil {
		return err
	}
	cfgDir := filepath.Join(home, ".config", "personal-review-bot")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte("ORG=acme\n"), 0o600); err != nil {
		return err
	}
	notes := "ADRs live in acme/architecture under adr/; accepted ones are binding.\nTasks are GitHub issues in acme/roadmap; a PR links its issue.\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "context.md"), []byte(notes), 0o644); err != nil {
		return err
	}

	cfg := config.LoadFrom(cfgDir)
	cfg.WebAddr = addr
	cfg.StateDir = filepath.Join(home, ".local", "state", "personal-review-bot")
	cfg.CacheDir = filepath.Join(home, ".cache", "personal-review-bot")
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := seedDemo(st, cfg.DBPath(), time.Now()); err != nil {
		return fmt.Errorf("seeding the demo: %w", err)
	}

	token, err := st.CSRFToken()
	if err != nil {
		return err
	}
	s := &server{d: pipeline.Deps{Store: st, Log: func(string, ...any) {}, Cfg: cfg, Live: config.NewLive(cfg)},
		token: token, hosts: cfg.AllowedHosts(), listen: addr, version: version, login: "octocat", updates: demoUpdater{},
		// Cached from the far future, so it never expires and GitHub is never asked.
		accounts: &accountCache{at: time.Now().Add(100 * 365 * 24 * time.Hour), acc: gh.Account{Login: "octocat",
			Name: "The Octocat", HTMLURL: "https://github.com/octocat", Orgs: []string{"acme", "acme-labs"}}},
		// A review page asks GitHub for the PR's labels and head; here they are what the demo stored.
		fetch: func(repo string, n int) (gh.PR, error) {
			r, err := st.Latest(repo, n)
			if err != nil || r == nil {
				return gh.PR{}, errors.Join(err, errors.New("not in the demo"))
			}
			return gh.PR{HeadSHA: r.HeadSHA, Labels: r.Labels}, nil
		},
		threads: func(string, int) ([]gh.Thread, error) { return nil, nil },
		send: func(*store.Review, string, []store.Finding, []pipeline.Reminder) (string, error) {
			return "", errors.New("the demo posts nothing")
		}}
	for _, h := range s.hosts {
		s.origins = append(s.origins, "http://"+h)
	}
	about = About{Version: version, Platform: "darwin/arm64", Repo: "https://github.com/" + cfg.UpdateRepo,
		ConfigDir: "~/.config/personal-review-bot", StateDir: "~/.local/state/personal-review-bot",
		Log: "~/Library/Logs/personal-review-bot.log"}
	header = s.headerView
	labelsColumn = func() string { return s.cfg().LabelsColumn() }

	srv := &http.Server{Addr: addr, Handler: s.guard(s.routes()), ReadHeaderTimeout: 5 * time.Second}
	go func() { // the bar says Watching while GitHub was checked lately, so the demo keeps "checking"
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = srv.Close()
				return
			case t := <-tick.C:
				_ = st.SetMeta("last_poll", t.Add(-20*time.Second).UTC().Format(time.RFC3339))
			}
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// demoUpdater makes the bar look like the daemon's (Watching, the version menu) without updating anything.
type demoUpdater struct{}

func (demoUpdater) Start() (string, error)    { return "", errors.New("the demo doesn't update") }
func (demoUpdater) CheckNow() (string, error) { return "Up to date.", nil }

// demoFinding is a finding as the demo seeds it: as proposed, then as the verifier left it.
type demoFinding struct {
	agent, severity, path string
	line                  int
	title, comment        string
	proposed, certainty   int
	verdict, status       string
	code                  string // the lines around it, as Finding.Context has them
}

type demoReview struct {
	repo           string
	number         int
	title, author  string
	tier, models   string
	reasons        string
	add, del, n    int
	cost           float64
	status         string
	decision       string
	labels         []string
	about          string
	approval       int
	approvalReason string
	findings       []demoFinding
	runs           []store.Run
	ago, took      time.Duration // when it started, and how long it ran (0: still running)
}

// seedDemo fills the store with the demo's reviews. The store stamps a review with the time it is written, so the
// times the demo needs are set afterwards, through the database itself.
func seedDemo(st *store.Store, dbPath string, now time.Time) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	const sha = "3f9c2a7e1b"
	run := func(agent, model string, findings, secs int, cost float64, checked string) store.Run {
		return store.Run{Agent: agent, Model: model, Findings: findings, Turns: 14, Duration: time.Duration(secs) * time.Second,
			CostUSD: cost, Checked: checked}
	}
	capture := "41\t \tfunc (c *Client) Capture(ctx context.Context, id string) error {\n" +
		"42\t+\t\tfor attempt := 0; attempt < 3; attempt++ {\n" +
		"43\t+\t\t\terr := c.gw.Capture(ctx, id)\n" +
		"44\t+\t\t\tif errors.Is(err, context.DeadlineExceeded) {\n" +
		"45\t+\t\t\t\tcontinue\n" +
		"46\t+\t\t\t}\n" +
		"47\t+\t\t\treturn err\n" +
		"48\t+\t\t}"
	logged := "51\t+\t\tif err != nil {\n52\t+\t\t\tlog.Printf(\"capture %s failed: %v\", id, err)\n53\t+\t\t}"

	reviews := []demoReview{
		{repo: "acme/infra", number: 228, title: "Split the worker pool by queue", author: "lena", tier: "large",
			models: "opus → opus", reasons: "risky paths: deploy/workers.tf · 640 lines in 14 files", add: 512, del: 128, n: 14,
			cost: 5.92, status: store.Done, decision: "you posted 2", labels: []string{"risk/high"}, ago: 26 * time.Hour, took: 9 * time.Minute},
		{repo: "acme/web", number: 981, title: "Lazy-load the analytics bundle", author: "marco", tier: "small",
			models: "haiku → sonnet", reasons: "38 lines in 3 files", add: 30, del: 8, n: 3, cost: 0.21, status: store.Done,
			decision: "approved by you", labels: []string{"risk/low"}, ago: 7 * time.Hour, took: 2 * time.Minute},
		{repo: "acme/payments", number: 409, title: "Rename the refund status enum", author: "priya", tier: "medium",
			models: "sonnet → opus", reasons: "212 lines in 9 files", add: 140, del: 72, n: 9, cost: 0.77, status: store.Done,
			decision: "bot posted 1, you posted 1", labels: []string{"risk/medium"}, ago: 5 * time.Hour, took: 4 * time.Minute},
		{repo: "acme/mobile", number: 77, title: "Fix typos in the onboarding guide", author: "sam", tier: "small",
			models: "haiku → sonnet", reasons: "docs only · 12 lines in 2 files", add: 6, del: 6, n: 2, cost: 0.04,
			status: store.Done, decision: "approved by you", labels: []string{"auto-risk/low"}, ago: 3 * time.Hour, took: time.Minute},
		{repo: "acme/web", number: 988, title: "Add dark mode to the account settings", author: "marco", tier: "small",
			models: "haiku → sonnet", reasons: "96 lines in 5 files", add: 81, del: 15, n: 5, cost: 0.18, status: store.AwaitingYou,
			decision: "no issues: approve?", labels: []string{"risk/low"},
			about:          "Adds a dark theme to the account settings and remembers the choice per user. The colours come from the design tokens, so the other pages can follow.",
			approval:       93,
			approvalReason: "Every changed component was rendered in both themes, and the stored preference is read once on load.",
			runs: []store.Run{run("logic", "haiku", 0, 48, 0.03, "Read `ThemeToggle.tsx` and how the preference is stored."),
				run("verifier", "sonnet", 0, 61, 0.09, ""), run("summary", "haiku", 0, 9, 0.01, "")},
			ago: 70 * time.Minute, took: 3 * time.Minute},
		{repo: "acme/payments", number: 412, title: "Retry card captures when the gateway times out", author: "priya",
			tier: "medium", models: "sonnet → opus", reasons: "risky paths: internal/gateway/ · 286 lines in 7 files",
			add: 231, del: 55, n: 7, cost: 1.42, status: store.AwaitingYou, decision: "2 need you", labels: []string{"risk/medium"},
			about:          "Retries a card capture up to three times when the payment gateway times out, instead of failing the order. Timeouts have been failing about one order in 400 since the gateway moved regions.",
			approval:       58,
			approvalReason: "The retry was traced end to end, but whether the gateway deduplicates a repeated capture isn't settled by the code.",
			findings: []demoFinding{
				{"logic", "high", "internal/gateway/capture.go", 44, "A retried capture can charge the card twice",
					"When the gateway times out after it has captured, the retry captures again: nothing makes the second call idempotent. Send the same `Idempotency-Key` on every attempt, as `Refund` already does (`refund.go:58`).",
					88, 91, "confirmed", store.Pending, capture},
				{"security", "low", "internal/gateway/capture.go", 52, "The gateway's error is logged with the card token",
					"The gateway echoes the card token in its error message, so this line writes it to the logs. Log the capture ID and the status code only.",
					80, 86, "confirmed", store.AutoPosted, logged},
				{agents.DesignAgent, "medium", "", 0, "Retrying inside the request ties up the checkout",
					"Three attempts with the gateway's 10s timeout keep the customer waiting up to 30s.\n\n**Instead:** queue the capture and retry from the existing `jobs` worker, as refunds do.\n\n**Evidence:** `adr/0012-async-payments.md` makes gateway calls asynchronous.",
					74, 74, "", store.Pending, ""},
			},
			runs: []store.Run{run("logic", "sonnet", 1, 212, 0.31, "Traced `Capture` from the checkout handler to the gateway client."),
				run("security", "sonnet", 1, 174, 0.24, "Read the error paths and what reaches the logs."),
				run("integration", "sonnet", 0, 160, 0.22, "Checked the gateway client's other callers in acme/billing."),
				run(agents.DesignAgent, "opus", 1, 245, 0.41, "Compared with how refunds retry, and adr/0012."),
				run("verifier", "opus", 2, 131, 0.21, ""), run("summary", "haiku", 0, 8, 0.01, "")},
			ago: 35 * time.Minute, took: 6 * time.Minute},
		{repo: "acme/infra", number: 231, title: "Move the queue workers to their own node pool", author: "lena", tier: "large",
			models: "opus → opus", reasons: "risky paths: deploy/nodepools.tf · 734 lines in 18 files", add: 601, del: 133, n: 18,
			cost: 2.10, status: store.Running, labels: []string{"risk/high"}, ago: 4 * time.Minute},
	}

	for _, r := range reviews {
		id, err := st.Insert(&store.Review{Repo: r.repo, Number: r.number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", r.repo, r.number),
			Title: r.title, Author: r.author, HeadSHA: sha, Status: store.Running, Source: "auto"})
		if err != nil {
			return err
		}
		err = errors.Join(st.SetPlan(id, r.title, r.author, sha, r.tier, r.models, r.reasons, r.add, r.del, r.n),
			st.AddCost(id, r.cost), st.SetLabels(id, r.labels))
		if r.about != "" {
			err = errors.Join(err, st.SetAbout(id, r.about), st.SetApproval(id, r.approval, r.approvalReason))
		}
		for _, f := range r.findings {
			inline := f.path != ""
			fid, ferr := st.AddFinding(&store.Finding{ReviewID: id, Agent: f.agent, Severity: f.severity, Path: f.path,
				Line: f.line, Title: f.title, Comment: f.comment, AgentConfidence: f.proposed, Status: store.Candidate, Inline: inline})
			err = errors.Join(err, ferr, st.UpdateFinding(&store.Finding{ID: fid, Line: f.line, Comment: f.comment,
				Confidence: f.certainty, Verdict: f.verdict, Status: f.status, Inline: inline}))
			if f.code != "" {
				err = errors.Join(err, st.SetContext(fid, f.code))
			}
		}
		for _, run := range r.runs {
			run.ReviewID = id
			err = errors.Join(err, st.AddRun(&run))
		}
		if r.status == store.Running {
			err = errors.Join(err, st.SetStage(id, "reviewing: opus (logic, security, integration, infra) + design (fable)"))
		} else {
			err = errors.Join(err, st.Finish(id, r.status, r.decision, "", ""))
		}
		started, finished := now.Add(-r.ago), ""
		if r.took > 0 {
			finished = started.Add(r.took).UTC().Format(time.RFC3339)
		}
		_, serr := db.Exec(`UPDATE reviews SET created_at = ?, started_at = ?, finished_at = ? WHERE id = ?`,
			started.UTC().Format(time.RFC3339), started.UTC().Format(time.RFC3339), finished, id)
		if err = errors.Join(err, serr); err != nil {
			return fmt.Errorf("%s#%d: %w", r.repo, r.number, err)
		}
	}

	// A finished review that shows what became of posted findings — the "edited" and downstream chips the page now adds.
	sid, serr := st.Insert(&store.Review{Repo: "acme/infra", Number: 231, URL: "https://github.com/acme/infra/pull/231",
		Title: "Cache the account lookups", Author: "lena", HeadSHA: sha, Status: store.Running, Source: "auto"})
	if serr != nil {
		return serr
	}
	dtime := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	post := func(agent, sev, path string, line int, title, authored, final, down, status string, cert int) error {
		fid, e := st.AddFinding(&store.Finding{ReviewID: sid, Agent: agent, Severity: sev, Path: path, Line: line,
			Title: title, Comment: authored, AgentConfidence: cert, Status: store.Candidate, Inline: path != ""})
		if e != nil {
			return e
		}
		e = st.UpdateFinding(&store.Finding{ID: fid, Line: line, Comment: final, Confidence: cert, Verdict: "confirmed",
			Status: status, Inline: path != "", PostedURL: fmt.Sprintf("https://github.com/acme/infra/pull/231#r%d", fid)})
		if down != "" {
			e = errors.Join(e, st.SetDownstream(fid, down, dtime))
		}
		return e
	}
	showErr := errors.Join(
		st.SetPlan(sid, "Cache the account lookups", "lena", sha, "medium", "sonnet → opus", "128 lines in 4 files", 128, 20, 4),
		st.AddCost(sid, 0.63), st.SetLabels(sid, []string{"risk/medium"}),
		// posted, you reworded it before sending, the author resolved it
		post("logic", "medium", "internal/account/cache.go", 40, "The cache never expires",
			"entries are kept forever",
			"Entries live forever, so a renamed account keeps serving stale data. Add a short TTL, as the token cache does.",
			"endorsed", store.Posted, 88),
		// posted as the bot wrote it, nothing happened since
		post("integration", "low", "internal/account/cache.go", 71, "Cache size is unbounded",
			"The map grows without a cap; a burst of distinct ids can hold memory for the process's life.",
			"The map grows without a cap; a burst of distinct ids can hold memory for the process's life.",
			"quiet", store.Posted, 83),
		// auto-posted, a colleague pushed back
		post("security", "low", "internal/account/cache.go", 12, "Lookup key is the raw email",
			"The cache key is the raw email, so any log line that dumps the key leaks it. Hash the key first.",
			"The cache key is the raw email, so any log line that dumps the key leaks it. Hash the key first.",
			"disputed", store.AutoPosted, 85),
		// a few auto-posts that held, so the proven floor reads realistically rather than 0
		post("logic", "low", "internal/account/cache.go", 58, "Error from the store is swallowed",
			"The error from `store.Get` is dropped; a miss then looks like an empty account. Return it.",
			"The error from `store.Get` is dropped; a miss then looks like an empty account. Return it.",
			"endorsed", store.AutoPosted, 90),
		post("integration", "low", "internal/account/cache.go", 63, "Metric name drifts from the others",
			"`account.cache.hit` breaks the `svc.<area>.<event>` convention the dashboards group on. Rename it.",
			"`account.cache.hit` breaks the `svc.<area>.<event>` convention the dashboards group on. Rename it.",
			"endorsed", store.AutoPosted, 87),
		post("infra", "low", "internal/account/cache.go", 90, "Cache built before the config is read",
			"The cache is constructed in `init`, before flags are parsed, so the size flag is ignored. Build it in `main`.",
			"The cache is constructed in `init`, before flags are parsed, so the size flag is ignored. Build it in `main`.",
			"quiet", store.AutoPosted, 84),
		st.Finish(sid, store.Done, "bot posted 4, you posted 2", "", ""),
	)
	if _, e := db.Exec(`UPDATE reviews SET created_at = ?, started_at = ?, finished_at = ? WHERE id = ?`,
		dtime, dtime, now.Add(-100*time.Minute).UTC().Format(time.RFC3339), sid); e != nil {
		showErr = errors.Join(showErr, e)
	}
	if showErr != nil {
		return fmt.Errorf("showcase review: %w", showErr)
	}

	usage, err := json.Marshal(agents.Usage{FiveHour: 0.23, FiveHourResets: now.Add(3 * time.Hour), Week: 0.41,
		WeekResets: now.Add(4 * 24 * time.Hour), Seen: now})
	if err != nil {
		return err
	}
	return errors.Join(st.SetMeta("usage", string(usage)), st.SetMeta("last_poll", now.Add(-20*time.Second).UTC().Format(time.RFC3339)))
}
