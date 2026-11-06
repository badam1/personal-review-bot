// Package store keeps reviews and their findings in a local SQLite database, shared by the daemon, the web
// page and the terminal UI (WAL mode, so the TUI can read while the daemon writes).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Review statuses.
const (
	Queued      = "queued"
	Running     = "running"
	AwaitingYou = "awaiting_you"
	Done        = "done"
	Failed      = "failed"
	Ignored     = "ignored"
)

// Finding statuses.
const (
	Candidate  = "candidate"   // proposed by a reviewer, not verified yet
	AutoPosted = "auto_posted" // verified ≥ threshold and posted by the bot
	WouldPost  = "would_post"  // dry run: would have been posted
	Pending    = "pending"     // waiting for you on the confirmation page
	Posted     = "posted"      // you chose to post it
	Discarded  = "discarded"   // you chose not to post it
	Rejected   = "rejected"    // the verifier rejected it
)

type Review struct {
	ID        int64
	Repo      string // owner/name
	Number    int
	URL       string
	Title     string
	Author    string
	HeadSHA   string
	Status    string
	Stage     string
	Tier      string
	Models    string
	Additions int
	Deletions int
	Files     int
	Source    string // auto | manual
	DryRun    bool
	Deep      bool   // you asked for a deep review: Opus and the design review whatever the PR's size
	Summary   string // the verifier's verdict on the findings
	About     string // what the PR changes and why, in two sentences, for the top of its page
	// Approval is how confident the verifier is (0-100) that nothing beyond its findings stands in the way of
	// approving, and ApprovalReason why ("" = it gave none).
	Approval       int
	ApprovalReason string
	Labels         []string // the PR's labels the page shows beside its size (SHOW_LABELS), e.g. risk/high
	Decision       string
	Error          string
	Reasons        string // why triage picked the tier and models
	Iteration      int    // which review of the PR this is: earlier ones that ran (dry runs and idle skips left out), plus one
	CostUSD        float64
	CreatedAt      time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
}

// Reran says whether the review is a re-review worth numbering: it ran (or will), it isn't a dry run, and it isn't
// the PR's first.
func (r Review) Reran() bool {
	return r.Iteration > 1 && !r.DryRun && (r.Tier != "" || r.Status == Queued || r.Status == Running)
}

type Finding struct {
	ID              int64
	ReviewID        int64
	Agent           string
	Path            string
	Line            int
	Severity        string
	Title           string
	Comment         string
	AgentConfidence int
	Confidence      int // after verification
	Verdict         string
	Status          string
	Inline          bool // line is inside the diff, so it can be an inline comment
	PostedURL       string
	Context         string    // the code around Line at review time: "line\tmark\ttext" rows, mark "+" for an added line
	Wrong           bool      // posted, then marked wrong by you
	Followup        string    // what a later review found: fixed | partly | open | moot | declined ("" = not checked)
	FollowupNote    string    // its evidence: a commit, a line, the author's reply
	FollowupReview  int64     // the review that checked it last
	RemindedSHA     string    // the PR's head when you last reminded the author it is still open ("" = never)
	RemindedAt      time.Time // when
}

// Follow-up statuses. Fixed, moot and declined are settled and not checked again.
const (
	FollowFixed    = "fixed"
	FollowPartly   = "partly"
	FollowOpen     = "open"
	FollowMoot     = "moot"
	FollowDeclined = "declined"
)

// Settled says whether a later review closed the finding (fixed, no longer applies, or the author declined).
func (f Finding) Settled() bool {
	return f.Followup == FollowFixed || f.Followup == FollowMoot || f.Followup == FollowDeclined
}

// Run is one Claude run within a review (a reviewer or the verifier) and what it did, so that a review that found
// nothing shows what was looked at.
type Run struct {
	ReviewID int64
	Agent    string // logic, security, …, or verifier
	Model    string
	Findings int
	Turns    int
	Denials  int // refused tool calls
	Duration time.Duration
	CostUSD  float64
	Checked  string // the reviewer's account of what it examined
	Error    string
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS reviews (
  id INTEGER PRIMARY KEY, repo TEXT NOT NULL, number INTEGER NOT NULL, url TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', head_sha TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL, stage TEXT NOT NULL DEFAULT '', tier TEXT NOT NULL DEFAULT '', models TEXT NOT NULL DEFAULT '',
  additions INTEGER NOT NULL DEFAULT 0, deletions INTEGER NOT NULL DEFAULT 0, files INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL DEFAULT 'auto', dry_run INTEGER NOT NULL DEFAULT 0, summary TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', reasons TEXT NOT NULL DEFAULT '', cost_usd REAL NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS reviews_pr ON reviews (repo, number, head_sha);
CREATE TABLE IF NOT EXISTS findings (
  id INTEGER PRIMARY KEY, review_id INTEGER NOT NULL REFERENCES reviews (id), agent TEXT NOT NULL,
  path TEXT NOT NULL, line INTEGER NOT NULL, severity TEXT NOT NULL, title TEXT NOT NULL, comment TEXT NOT NULL,
  agent_confidence INTEGER NOT NULL, confidence INTEGER NOT NULL DEFAULT 0, verdict TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL, inline INTEGER NOT NULL DEFAULT 0, posted_url TEXT NOT NULL DEFAULT '',
  context TEXT NOT NULL DEFAULT '', wrong INTEGER NOT NULL DEFAULT 0, followup TEXT NOT NULL DEFAULT '',
  followup_note TEXT NOT NULL DEFAULT '', followup_review INTEGER NOT NULL DEFAULT 0,
  reminded_sha TEXT NOT NULL DEFAULT '', reminded_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY, review_id INTEGER NOT NULL REFERENCES reviews (id), agent TEXT NOT NULL, model TEXT NOT NULL,
  findings INTEGER NOT NULL DEFAULT 0, turns INTEGER NOT NULL DEFAULT 0, denials INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0, checked TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS ignored_prs (repo TEXT NOT NULL, number INTEGER NOT NULL, PRIMARY KEY (repo, number));
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init database: %w", err)
	}
	// Findings from before this were scored on one mixed confidence, not certainty and severity apart; Decisions
	// leaves them out.
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('scoring_since', ?)`, now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("init database: %w", err)
	}
	for _, c := range []struct{ table, column, def string }{
		{"reviews", "reasons", "TEXT NOT NULL DEFAULT ''"},
		{"reviews", "deep", "INTEGER NOT NULL DEFAULT 0"},
		{"reviews", "about", "TEXT NOT NULL DEFAULT ''"},
		{"reviews", "approval", "INTEGER NOT NULL DEFAULT 0"},
		{"reviews", "approval_reason", "TEXT NOT NULL DEFAULT ''"},
		{"reviews", "labels", "TEXT NOT NULL DEFAULT ''"},
		{"findings", "context", "TEXT NOT NULL DEFAULT ''"},
		{"findings", "wrong", "INTEGER NOT NULL DEFAULT 0"},
		{"findings", "followup", "TEXT NOT NULL DEFAULT ''"},
		{"findings", "followup_note", "TEXT NOT NULL DEFAULT ''"},
		{"findings", "followup_review", "INTEGER NOT NULL DEFAULT 0"},
		{"findings", "reminded_sha", "TEXT NOT NULL DEFAULT ''"},
		{"findings", "reminded_at", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumn(db, c.table, c.column, c.def); err != nil {
			db.Close()
			return nil, fmt.Errorf("init database: %w", err)
		}
	}
	// The design reviewer was called the approach reviewer at first; its earlier findings and runs follow the name.
	// An approval was recorded as "you approved" at first; it reads "approved by you" now.
	if _, err := db.Exec(`UPDATE findings SET agent = 'design' WHERE agent = 'approach';
	  UPDATE findings SET verdict = 'design' WHERE verdict = 'approach';
	  UPDATE runs SET agent = 'design' WHERE agent = 'approach';
	  UPDATE reviews SET decision = replace(decision, 'you approved', 'approved by you') WHERE decision LIKE '%you approved%';`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init database: %w", err)
	}
	// Findings used to be marked auto_posted before the post to GitHub; when it failed, the review failed but they kept
	// the status with no address, and read as comments the bot left. They were never posted: they are pending.
	if _, err := db.Exec(`UPDATE findings SET status = ? WHERE status = ? AND posted_url = ''
	  AND review_id IN (SELECT id FROM reviews WHERE status = ?)`, Pending, AutoPosted, Failed); err != nil {
		db.Close()
		return nil, fmt.Errorf("init database: %w", err)
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// addColumn adds a column that databases created before it was in the schema lack.
func addColumn(db *sql.DB, table, column, def string) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

// ── meta ─────────────────────────────────────────────────────────────────────

func (s *Store) Meta(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// CSRFToken returns the secret the confirmation page must send back with every action.
func (s *Store) CSRFToken() (string, error) {
	if t := s.Meta("csrf"); t != "" {
		return t, nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	return t, s.SetMeta("csrf", t)
}

// ── reviews ──────────────────────────────────────────────────────────────────

const reviewCols = `id, repo, number, url, title, author, head_sha, status, stage, tier, models, additions, deletions,
  files, source, dry_run, deep, summary, about, approval, approval_reason, labels, decision, error, reasons, cost_usd, created_at, started_at, finished_at,
  (SELECT count(*) FROM reviews p WHERE p.repo = reviews.repo AND p.number = reviews.number AND p.id < reviews.id
    AND p.tier <> '' AND p.dry_run = 0) + 1`

func scanReview(row interface{ Scan(...any) error }) (*Review, error) {
	var r Review
	var dry, deep int
	var created, started, finished, labels string
	err := row.Scan(&r.ID, &r.Repo, &r.Number, &r.URL, &r.Title, &r.Author, &r.HeadSHA, &r.Status, &r.Stage, &r.Tier,
		&r.Models, &r.Additions, &r.Deletions, &r.Files, &r.Source, &dry, &deep, &r.Summary, &r.About, &r.Approval, &r.ApprovalReason, &labels, &r.Decision, &r.Error,
		&r.Reasons, &r.CostUSD, &created, &started, &finished, &r.Iteration)
	if err != nil {
		return nil, err
	}
	r.DryRun, r.Deep, r.Labels = dry == 1, deep == 1, strings.Fields(labels)
	r.CreatedAt, r.StartedAt, r.FinishedAt = parseTime(created), parseTime(started), parseTime(finished)
	return &r, nil
}

func (s *Store) queryReviews(where string, args ...any) ([]Review, error) {
	rows, err := s.db.Query(`SELECT `+reviewCols+` FROM reviews `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) Insert(r *Review) (int64, error) {
	dry, deep := 0, 0
	if r.DryRun {
		dry = 1
	}
	if r.Deep {
		deep = 1
	}
	res, err := s.db.Exec(`INSERT INTO reviews (repo, number, url, title, author, head_sha, status, source, dry_run,
	  deep, decision, created_at, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Number, r.URL, r.Title, r.Author, r.HeadSHA, r.Status, r.Source, dry, deep, r.Decision, now(),
		map[bool]string{true: now(), false: ""}[r.Status == Running])
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Get(id int64) (*Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE id = ?`, id))
}

// Latest returns the most recent review of a PR, or nil if it was never seen.
func (s *Store) Latest(repo string, number int) (*Review, error) {
	r, err := scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE repo = ? AND number = ? ORDER BY id DESC LIMIT 1`, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// PreviousComplete returns the PR's latest review before id in which every reviewer ran (nil if there is none), so
// a re-review knows which commit was last covered.
func (s *Store) PreviousComplete(repo string, number int, before int64) (*Review, error) {
	r, err := scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE repo = ? AND number = ? AND id < ?
	  AND status IN (?, ?) AND tier <> '' AND error = '' ORDER BY id DESC LIMIT 1`, repo, number, before, Done, AwaitingYou))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ClaimNext marks the oldest queued review as running and returns it (nil if the queue is empty).
func (s *Store) ClaimNext() (*Review, error) {
	var id int64
	err := s.db.QueryRow(`UPDATE reviews SET status = ?, started_at = ?, stage = 'starting'
	  WHERE id = (SELECT id FROM reviews WHERE status = ? ORDER BY id LIMIT 1) RETURNING id`, Running, now(), Queued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// ResetRunning re-queues reviews that were interrupted (daemon restart).
func (s *Store) ResetRunning() error {
	_, err := s.db.Exec(`UPDATE reviews SET status = ?, stage = '' WHERE status = ?`, Queued, Running)
	return err
}

func (s *Store) Recent(n int) ([]Review, error) { return s.queryReviews(`ORDER BY id DESC LIMIT ?`, n) }

// HistoryFilter narrows the review history; an empty field matches everything. Status "active" means queued or running.
type HistoryFilter struct{ Repo, Author, Status string }

// History returns one page of reviews, newest first, and how many match in all.
func (s *Store) History(f HistoryFilter, limit, offset int) ([]Review, int, error) {
	where, args := "WHERE 1 = 1", []any{}
	if f.Repo != "" {
		where, args = where+" AND repo = ?", append(args, f.Repo)
	}
	if f.Author != "" {
		where, args = where+" AND author = ?", append(args, f.Author)
	}
	switch f.Status {
	case "":
	case "active":
		where, args = where+" AND status IN (?, ?)", append(args, Queued, Running)
	default:
		where, args = where+" AND status = ?", append(args, f.Status)
	}
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM reviews `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rs, err := s.queryReviews(where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	return rs, total, err
}

// Distinct returns every repo or author seen, for the history filters.
func (s *Store) Distinct(column string) ([]string, error) {
	if column != "repo" && column != "author" {
		return nil, fmt.Errorf("no filter on %q", column)
	}
	rows, err := s.db.Query(`SELECT DISTINCT ` + column + ` FROM reviews WHERE ` + column + ` <> '' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PRReviews returns every review of one PR, newest first.
func (s *Store) PRReviews(repo string, number int) ([]Review, error) {
	return s.queryReviews(`WHERE repo = ? AND number = ? ORDER BY id DESC`, repo, number)
}

func (s *Store) Active() ([]Review, error) {
	return s.queryReviews(`WHERE status IN (?, ?) ORDER BY id`, Queued, Running)
}

func (s *Store) AwaitingYou() ([]Review, error) {
	return s.queryReviews(`WHERE status = ? ORDER BY id DESC`, AwaitingYou)
}

// Failures returns the PRs whose latest review failed since the given time, newest first: the ones worth retrying.
// Ignored PRs and dry runs are left out.
func (s *Store) Failures(since time.Time) ([]Review, error) {
	return s.queryReviews(`WHERE status = ? AND dry_run = 0 AND created_at >= ?
	  AND id IN (SELECT max(id) FROM reviews GROUP BY repo, number)
	  AND NOT EXISTS (SELECT 1 FROM ignored_prs i WHERE i.repo = reviews.repo AND i.number = reviews.number)
	  ORDER BY id DESC`, Failed, since.UTC().Format(time.RFC3339))
}

func (s *Store) SetStage(id int64, stage string) error {
	_, err := s.db.Exec(`UPDATE reviews SET stage = ? WHERE id = ?`, stage, id)
	return err
}

// SetAbout records what the PR changes and why, as the summary wrote it.
func (s *Store) SetAbout(id int64, about string) error {
	_, err := s.db.Exec(`UPDATE reviews SET about = ? WHERE id = ?`, about, id)
	return err
}

// SetLabels records the PR's labels the page shows.
func (s *Store) SetLabels(id int64, labels []string) error {
	_, err := s.db.Exec(`UPDATE reviews SET labels = ? WHERE id = ?`, strings.Join(labels, " "), id)
	return err
}

// SetApproval records the verifier's confidence in approving, and why.
func (s *Store) SetApproval(id int64, confidence int, reason string) error {
	_, err := s.db.Exec(`UPDATE reviews SET approval = ?, approval_reason = ? WHERE id = ?`, confidence, reason, id)
	return err
}

// SetPlan records what the PR looks like now and how it will be reviewed, and why.
func (s *Store) SetPlan(id int64, title, author, sha, tier, models, reasons string, additions, deletions, files int) error {
	_, err := s.db.Exec(`UPDATE reviews SET title = ?, author = ?, head_sha = ?, tier = ?, models = ?, reasons = ?,
	  additions = ?, deletions = ?, files = ? WHERE id = ?`, title, author, sha, tier, models, reasons, additions, deletions,
		files, id)
	return err
}

func (s *Store) AddCost(id int64, usd float64) error {
	_, err := s.db.Exec(`UPDATE reviews SET cost_usd = cost_usd + ? WHERE id = ?`, usd, id)
	return err
}

func (s *Store) Finish(id int64, status, decision, summary, errText string) error {
	_, err := s.db.Exec(`UPDATE reviews SET status = ?, stage = '', decision = ?, summary = CASE WHEN ? = '' THEN summary ELSE ? END,
	  error = ?, finished_at = ? WHERE id = ?`, status, decision, summary, summary, errText, now(), id)
	return err
}

// Supersede closes the PR's older reviews that are still waiting for you, once review keep has replaced them. Their
// undecided findings stay pending, so later re-reviews still see them as never decided.
func (s *Store) Supersede(repo string, number int, keep int64) error {
	_, err := s.db.Exec(`UPDATE reviews SET status = ?, stage = '', decision = ?, finished_at = ?
	  WHERE repo = ? AND number = ? AND status = ? AND id < ?`,
		Done, fmt.Sprintf("superseded by review %d", keep), now(), repo, number, AwaitingYou, keep)
	return err
}

// ── runs ─────────────────────────────────────────────────────────────────────

func (s *Store) AddRun(r *Run) error {
	_, err := s.db.Exec(`INSERT INTO runs (review_id, agent, model, findings, turns, denials, duration_ms, cost_usd, checked,
	  error) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.ReviewID, r.Agent, r.Model, r.Findings, r.Turns, r.Denials,
		r.Duration.Milliseconds(), r.CostUSD, r.Checked, r.Error)
	return err
}

// Runs returns a review's reviewer runs in name order, then the verifier.
func (s *Store) Runs(reviewID int64) ([]Run, error) {
	rows, err := s.db.Query(`SELECT review_id, agent, model, findings, turns, denials, duration_ms, cost_usd, checked, error
	  FROM runs WHERE review_id = ? ORDER BY agent = 'verifier', agent, id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var ms int64
		if err := rows.Scan(&r.ReviewID, &r.Agent, &r.Model, &r.Findings, &r.Turns, &r.Denials, &ms, &r.CostUSD,
			&r.Checked, &r.Error); err != nil {
			return nil, err
		}
		r.Duration = time.Duration(ms) * time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── ignored PRs ──────────────────────────────────────────────────────────────

func (s *Store) IsIgnored(repo string, number int) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM ignored_prs WHERE repo = ? AND number = ?`, repo, number).Scan(&n)
	return n > 0
}

func (s *Store) IgnorePR(repo string, number int) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO ignored_prs (repo, number) VALUES (?, ?)`, repo, number)
	return err
}

// UnignorePR lets the PR be reviewed again; its next activity (a re-request or new commits) brings it back.
func (s *Store) UnignorePR(repo string, number int) error {
	_, err := s.db.Exec(`DELETE FROM ignored_prs WHERE repo = ? AND number = ?`, repo, number)
	return err
}

// IgnoredPRs returns the ignored PRs with their latest review (for the title and author), newest first.
func (s *Store) IgnoredPRs() ([]Review, error) {
	return s.queryReviews(`WHERE id IN (SELECT max(r.id) FROM reviews r JOIN ignored_prs i ON i.repo = r.repo AND
	  i.number = r.number GROUP BY r.repo, r.number) ORDER BY id DESC`)
}

// ── findings ─────────────────────────────────────────────────────────────────

// DecisionRow counts what became of the findings in one score range.
type DecisionRow struct {
	Label                        string
	BotPosted, Posted, Discarded int
}

// Decisions shows, per certainty range (the top one split by severity), what became of the findings scored since
// the verifier started rating certainty and severity apart. Whether the auto-post rule fits is read off this. The
// design reviewer's concerns, which carry no certainty, get a row of their own: whether it earns its cost.
func (s *Store) Decisions(threshold int) (time.Time, []DecisionRow, error) {
	since := s.Meta("scoring_since")
	rows := []DecisionRow{
		{Label: fmt.Sprintf("≥ %d%% · high", threshold)},
		{Label: fmt.Sprintf("≥ %d%% · medium", threshold)},
		{Label: fmt.Sprintf("≥ %d%% · low", threshold)},
		{Label: fmt.Sprintf("%d–%d%%", threshold-20, threshold-1)},
		{Label: fmt.Sprintf("%d–%d%%", threshold-40, threshold-21)},
		{Label: fmt.Sprintf("< %d%%", threshold-40)},
		{Label: "design concerns"},
	}
	q, err := s.db.Query(`SELECT CASE WHEN f.agent = 'design' THEN 6 WHEN f.confidence >= ? THEN CASE f.severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END
	  WHEN f.confidence >= ? THEN 3 WHEN f.confidence >= ? THEN 4 ELSE 5 END AS bucket, f.status, count(*)
	  FROM findings f JOIN reviews r ON r.id = f.review_id
	  WHERE r.created_at >= ? AND r.dry_run = 0 AND f.status IN (?, ?, ?) GROUP BY bucket, f.status`,
		threshold, threshold-20, threshold-40, since, AutoPosted, Posted, Discarded)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer q.Close()
	for q.Next() {
		var bucket, n int
		var status string
		if err := q.Scan(&bucket, &status, &n); err != nil {
			return time.Time{}, nil, err
		}
		switch status {
		case AutoPosted:
			rows[bucket].BotPosted = n
		case Posted:
			rows[bucket].Posted = n
		case Discarded:
			rows[bucket].Discarded = n
		}
	}
	return parseTime(since), rows, q.Err()
}

const findingCols = `id, review_id, agent, path, line, severity, title, comment, agent_confidence, confidence, verdict,
  status, inline, posted_url, context, wrong, followup, followup_note, followup_review, reminded_sha, reminded_at`

func (s *Store) AddFinding(f *Finding) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO findings (review_id, agent, path, line, severity, title, comment, agent_confidence,
	  confidence, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ReviewID, f.Agent, f.Path, f.Line, f.Severity, f.Title, f.Comment, f.AgentConfidence, f.Confidence, f.Status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateFinding(f *Finding) error {
	inline := 0
	if f.Inline {
		inline = 1
	}
	_, err := s.db.Exec(`UPDATE findings SET line = ?, comment = ?, confidence = ?, verdict = ?, status = ?, inline = ?,
	  posted_url = ? WHERE id = ?`, f.Line, f.Comment, f.Confidence, f.Verdict, f.Status, inline, f.PostedURL, f.ID)
	return err
}

// SetContext stores the code around a finding's line, as it was when reviewed.
func (s *Store) SetContext(id int64, context string) error {
	_, err := s.db.Exec(`UPDATE findings SET context = ? WHERE id = ?`, context, id)
	return err
}

// SetWrong marks a posted finding as wrong (or takes the mark back); only posted findings can be marked.
func (s *Store) SetWrong(id int64, wrong bool) error {
	res, err := s.db.Exec(`UPDATE findings SET wrong = ? WHERE id = ? AND status IN (?, ?)`, wrong, id, AutoPosted, Posted)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finding %d was not posted", id)
	}
	return nil
}

// SetFollowup records what a later review found about a posted finding.
func (s *Store) SetFollowup(id int64, status, note string, reviewID int64) error {
	_, err := s.db.Exec(`UPDATE findings SET followup = ?, followup_note = ?, followup_review = ? WHERE id = ?`,
		status, note, reviewID, id)
	return err
}

// SetReminded records that you reminded the author, at the PR's head sha, that a finding is still open.
func (s *Store) SetReminded(id int64, sha string) error {
	_, err := s.db.Exec(`UPDATE findings SET reminded_sha = ?, reminded_at = ? WHERE id = ?`, sha, now(), id)
	return err
}

// EarlierPosted returns the findings posted (by the bot or by you) on the PR's reviews before review before, dry runs
// left out, oldest first.
func (s *Store) EarlierPosted(repo string, number int, before int64) ([]Finding, error) {
	return s.queryFindings(`SELECT f.id, f.review_id, f.agent, f.path, f.line, f.severity, f.title, f.comment,
	  f.agent_confidence, f.confidence, f.verdict, f.status, f.inline, f.posted_url, f.context, f.wrong, f.followup,
	  f.followup_note, f.followup_review, f.reminded_sha, f.reminded_at
	  FROM findings f JOIN reviews r ON r.id = f.review_id
	  WHERE r.repo = ? AND r.number = ? AND r.id < ? AND r.dry_run = 0 AND f.status IN (?, ?) ORDER BY f.id`,
		repo, number, before, AutoPosted, Posted)
}

// Finding returns one finding.
func (s *Store) Finding(id int64) (*Finding, error) {
	fs, err := s.queryFindings(`SELECT `+findingCols+` FROM findings WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(fs) == 0 {
		return nil, sql.ErrNoRows
	}
	return &fs[0], nil
}

func (s *Store) Findings(reviewID int64) ([]Finding, error) {
	return s.queryFindings(`SELECT `+findingCols+` FROM findings WHERE review_id = ? ORDER BY confidence DESC, id`, reviewID)
}

// PriorFindings returns what earlier reviews of the same PR found and what happened to it (for re-reviews).
func (s *Store) PriorFindings(repo string, number int, before int64) ([]Finding, error) {
	return s.queryFindings(`SELECT f.id, f.review_id, f.agent, f.path, f.line, f.severity, f.title, f.comment,
	  f.agent_confidence, f.confidence, f.verdict, f.status, f.inline, f.posted_url, f.context, f.wrong, f.followup,
	  f.followup_note, f.followup_review, f.reminded_sha, f.reminded_at
	  FROM findings f JOIN reviews r ON r.id = f.review_id
	  WHERE r.repo = ? AND r.number = ? AND r.id < ? AND f.status IN (?, ?, ?, ?)
	  ORDER BY f.id DESC LIMIT 50`, repo, number, before, AutoPosted, Posted, Discarded, Pending)
}

func (s *Store) queryFindings(query string, args ...any) ([]Finding, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		var inline, wrong int
		var reminded string
		if err := rows.Scan(&f.ID, &f.ReviewID, &f.Agent, &f.Path, &f.Line, &f.Severity, &f.Title, &f.Comment,
			&f.AgentConfidence, &f.Confidence, &f.Verdict, &f.Status, &inline, &f.PostedURL, &f.Context, &wrong,
			&f.Followup, &f.FollowupNote, &f.FollowupReview, &f.RemindedSHA, &reminded); err != nil {
			return nil, err
		}
		f.Inline, f.Wrong, f.RemindedAt = inline == 1, wrong == 1, parseTime(reminded)
		out = append(out, f)
	}
	return out, rows.Err()
}
