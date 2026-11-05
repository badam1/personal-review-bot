package store

import (
	"sort"
	"time"
)

// Statistics for the stats page. Dry runs are left out everywhere: they posted nothing and were trials.

// Totals sums up the reviews that ran since a time.
type Totals struct {
	Reviews, PRs, Approvals                   int
	BotPosted, YouPosted, Discarded, Rejected int
	Wrong                                     int // posted, then marked wrong by you
	CostUSD                                   float64
}

// PerReview is the average cost of one review.
func (t Totals) PerReview() float64 {
	if t.Reviews == 0 {
		return 0
	}
	return t.CostUSD / float64(t.Reviews)
}

// Posted counts findings that reached the PR, by the bot or by you.
func (t Totals) Posted() int { return t.BotPosted + t.YouPosted }

func (s *Store) Totals(since time.Time) (Totals, error) {
	var t Totals
	at := since.UTC().Format(time.RFC3339)
	err := s.db.QueryRow(`SELECT count(*), count(DISTINCT repo || '#' || number), coalesce(sum(cost_usd), 0),
	  coalesce(sum(decision LIKE '%approved%'), 0)
	  FROM reviews WHERE dry_run = 0 AND tier <> '' AND created_at >= ?`, at).Scan(&t.Reviews, &t.PRs, &t.CostUSD, &t.Approvals)
	if err != nil {
		return t, err
	}
	err = s.db.QueryRow(`SELECT coalesce(sum(f.status = ?), 0), coalesce(sum(f.status = ?), 0), coalesce(sum(f.status = ?), 0),
	  coalesce(sum(f.status = ?), 0), coalesce(sum(f.wrong), 0)
	  FROM findings f JOIN reviews r ON r.id = f.review_id WHERE r.dry_run = 0 AND r.created_at >= ?`,
		AutoPosted, Posted, Discarded, Rejected, at).Scan(&t.BotPosted, &t.YouPosted, &t.Discarded, &t.Rejected, &t.Wrong)
	return t, err
}

// ReviewerRow is what one reviewer (or the verifier) has done and cost.
type ReviewerRow struct {
	Agent                                     string
	Runs, Failed, Found                       int
	BotPosted, YouPosted, Discarded, Rejected int
	Wrong                                     int // posted, then marked wrong by you
	Checked, Fixed                            int // posted findings a later review checked, and those it found fixed
	CostUSD                                   float64
}

func (r ReviewerRow) Posted() int { return r.BotPosted + r.YouPosted }

// FixedShare is the share of checked findings that were fixed (0-1; 0 when none was checked).
func (r ReviewerRow) FixedShare() float64 {
	if r.Checked == 0 {
		return 0
	}
	return float64(r.Fixed) / float64(r.Checked)
}

// Right is the share of posted findings you haven't marked wrong (0-1; 0 when nothing was posted).
func (r ReviewerRow) Right() float64 {
	if r.Posted() == 0 {
		return 0
	}
	return float64(r.Posted()-r.Wrong) / float64(r.Posted())
}

// CostPerPosted is what each finding that reached a PR cost (0 when none did).
func (r ReviewerRow) CostPerPosted() float64 {
	if r.Posted() == 0 {
		return 0
	}
	return r.CostUSD / float64(r.Posted())
}

// Reviewers returns every reviewer's runs, findings and their fate, most expensive first, the verifier last.
func (s *Store) Reviewers() ([]ReviewerRow, error) {
	byAgent := map[string]*ReviewerRow{}
	row := func(agent string) *ReviewerRow {
		if byAgent[agent] == nil {
			byAgent[agent] = &ReviewerRow{Agent: agent}
		}
		return byAgent[agent]
	}
	q, err := s.db.Query(`SELECT ru.agent, count(*), sum(ru.error <> ''), sum(ru.cost_usd)
	  FROM runs ru JOIN reviews r ON r.id = ru.review_id WHERE r.dry_run = 0 GROUP BY ru.agent`)
	if err != nil {
		return nil, err
	}
	for q.Next() {
		var agent string
		var runs, failed int
		var cost float64
		if err := q.Scan(&agent, &runs, &failed, &cost); err != nil {
			q.Close()
			return nil, err
		}
		r := row(agent)
		r.Runs, r.Failed, r.CostUSD = runs, failed, cost
	}
	q.Close()
	q, err = s.db.Query(`SELECT f.agent, count(*), sum(f.status = ?), sum(f.status = ?), sum(f.status = ?), sum(f.status = ?),
	  sum(f.wrong), sum(f.followup <> ''), sum(f.followup = ?)
	  FROM findings f JOIN reviews r ON r.id = f.review_id WHERE r.dry_run = 0 GROUP BY f.agent`,
		AutoPosted, Posted, Discarded, Rejected, FollowFixed)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var agent string
		var found, bot, you, discarded, rejected, wrong, checked, fixed int
		if err := q.Scan(&agent, &found, &bot, &you, &discarded, &rejected, &wrong, &checked, &fixed); err != nil {
			return nil, err
		}
		r := row(agent)
		r.Found, r.BotPosted, r.YouPosted, r.Discarded, r.Rejected, r.Wrong = found, bot, you, discarded, rejected, wrong
		r.Checked, r.Fixed = checked, fixed
	}
	out := make([]ReviewerRow, 0, len(byAgent))
	for _, r := range byAgent {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := checker(out[i].Agent), checker(out[j].Agent); a != b {
			return b
		}
		return out[i].CostUSD > out[j].CostUSD
	})
	return out, q.Err()
}

// checker says whether the agent finds nothing of its own: it checks other findings (the verifier, the follow-up) or
// summarises the PR.
func checker(agent string) bool {
	return agent == "verifier" || agent == "followup" || agent == "summary"
}

// DaySpend is what the reviews created on one (local) day cost.
type DaySpend struct {
	Day     time.Time
	Reviews int
	CostUSD float64
}

// DailySpend returns the last days days up to now, oldest first, with empty days as zero.
func (s *Store) DailySpend(days int, now time.Time) ([]DaySpend, error) {
	now = now.Local()
	first := time.Date(now.Year(), now.Month(), now.Day()-days+1, 0, 0, 0, 0, time.Local)
	out := make([]DaySpend, days)
	for i := range out {
		out[i].Day = first.AddDate(0, 0, i)
	}
	q, err := s.db.Query(`SELECT created_at, cost_usd FROM reviews WHERE dry_run = 0 AND tier <> '' AND created_at >= ?`,
		first.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var created string
		var cost float64
		if err := q.Scan(&created, &cost); err != nil {
			return nil, err
		}
		t := parseTime(created).Local()
		i := int(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).Sub(first).Hours()+12) / 24
		if i >= 0 && i < days {
			out[i].Reviews++
			out[i].CostUSD += cost
		}
	}
	return out, q.Err()
}

// CostRow is the runs and spend of one model, or the reviews and spend of one tier or repo.
type CostRow struct {
	Name    string
	Count   int
	Posted  int // findings that reached the PR (repos only)
	CostUSD float64
}

// Average is the spend per run (models) or per review (tiers, repos).
func (r CostRow) Average() float64 {
	if r.Count == 0 {
		return 0
	}
	return r.CostUSD / float64(r.Count)
}

// Models returns runs and spend per model, most expensive first.
func (s *Store) Models() ([]CostRow, error) {
	return s.costRows(`SELECT ru.model, count(*), 0, sum(ru.cost_usd) FROM runs ru JOIN reviews r ON r.id = ru.review_id
	  WHERE r.dry_run = 0 GROUP BY ru.model ORDER BY sum(ru.cost_usd) DESC`)
}

// Tiers returns reviews and spend per PR size, and for deep reviews (whatever the size), which come last.
func (s *Store) Tiers() ([]CostRow, error) {
	return s.costRows(`SELECT tier, count(*), 0, sum(cost_usd) FROM reviews WHERE dry_run = 0 AND tier <> ''
	  GROUP BY tier ORDER BY CASE tier WHEN 'small' THEN 0 WHEN 'medium' THEN 1 WHEN 'large' THEN 2 ELSE 3 END`)
}

// Repos returns the repos with the most reviews: reviews, findings that reached the PR, and spend.
func (s *Store) Repos(limit int) ([]CostRow, error) {
	return s.costRows(`SELECT r.repo, count(*), coalesce(sum((SELECT count(*) FROM findings f WHERE f.review_id = r.id
	  AND f.status IN ('auto_posted', 'posted'))), 0), sum(r.cost_usd)
	  FROM reviews r WHERE r.dry_run = 0 AND r.tier <> '' GROUP BY r.repo ORDER BY count(*) DESC, sum(r.cost_usd) DESC
	  LIMIT ?`, limit)
}

func (s *Store) costRows(query string, args ...any) ([]CostRow, error) {
	q, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []CostRow
	for q.Next() {
		var r CostRow
		if err := q.Scan(&r.Name, &r.Count, &r.Posted, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, q.Err()
}
