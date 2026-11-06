// Package daemon is the long-running bot: it polls GitHub for review requests addressed to the user directly,
// runs reviews on a small worker pool, and serves the confirmation page.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/downstream"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/update"
	"github.com/badam1/personal-review-bot/internal/version"
	"github.com/badam1/personal-review-bot/internal/web"
)

func Run(ctx context.Context, cfg config.Config) error {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	client, err := gh.New()
	if err != nil {
		return err
	}
	d := pipeline.Deps{Cfg: cfg, Live: config.NewLive(cfg), Store: st, GH: client, Log: log.Printf}
	auto := cfg.AutoPostSeverities()
	if auto == "" {
		auto = "no severity"
	}
	// The TUI runs without a GitHub client, so the login is left here for it to read.
	_ = st.SetMeta("login", client.Login)
	log.Printf("personal-review-bot %s started for @%s (org %q, every %s, auto-post ≥ %d%% for %s, page %s)",
		version.String(), client.Login, cfg.Org, cfg.PollInterval, cfg.Threshold, auto, cfg.WebURL())

	if err := st.ResetRunning(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	upd := newUpdater(d, client, exe)
	if done, ok := update.Started(st, version.String()); ok { // a start ends whatever update was under way
		log.Printf("updated from %s to %s", done.From, done.To)
	}
	go upd.loop(ctx)
	if st.Meta("seeded") == "" {
		if err := seed(d); err != nil {
			return fmt.Errorf("first run: %w", err)
		}
	}

	go func() {
		if err := web.Serve(ctx, d, upd); err != nil {
			log.Printf("confirmation page stopped: %v", err)
		}
	}()
	for i := range max(cfg.MaxParallel, maxWorkers) {
		go worker(ctx, d, upd, i)
	}

	interval := cfg.PollInterval
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		cur := d.Current()
		poll(cur)
		pollDownstream(ctx, cur)
		if cur.Cfg.PollInterval != interval { // changed on the Settings page
			interval = cur.Cfg.PollInterval
			tick.Reset(interval)
		}
		select {
		case <-ctx.Done():
			log.Printf("personal-review-bot stopping")
			return nil
		case <-upd.drain:
			if upd.waitIdle(ctx) != nil {
				log.Printf("personal-review-bot stopping")
				return nil
			}
			log.Printf("personal-review-bot restarting into %s", upd.version())
			update.SetState(st, "restarting into "+upd.version())
			update.Restarting(st, version.String())
			return ErrRestart
		case <-tick.C:
		}
	}
}

// How often a posted finding's downstream state is re-read, and how many to check per pass. The cutoff makes this
// self-throttling: called every tick, PostedToCheck returns only findings not looked at within the window, so a quiet
// run does nothing and a fresh post is picked up on the next tick.
const (
	downstreamEvery = 6 * time.Hour
	downstreamBatch = 50
)

// ghDownstream adapts the GitHub client to downstream.Checker, the one seam onto the API.
type ghDownstream struct{ c *gh.Client }

func (g ghDownstream) Threads(_ context.Context, repo string, number int) ([]downstream.Thread, error) {
	return g.c.ReviewThreadStates(repo, number)
}

// pollDownstream re-reads what became of the findings the bot posted (resolved, argued with, reacted to) and records
// it, so the measurement layer can tell an auto-post that landed from one that didn't.
func pollDownstream(ctx context.Context, d pipeline.Deps) {
	before := time.Now().UTC().Add(-downstreamEvery).Format(time.RFC3339)
	due, err := d.Store.PostedToCheck(before, downstreamBatch)
	if err != nil {
		log.Printf("downstream: %v", err)
		return
	}
	if len(due) == 0 {
		return
	}
	posted := make([]downstream.Posted, len(due))
	for i, f := range due {
		posted[i] = downstream.Posted{ID: f.ID, Repo: f.Repo, Number: f.Number, Path: f.Path, Line: f.Line}
	}
	for _, r := range downstream.Poll(ctx, posted, ghDownstream{d.GH}, log.Printf) {
		if err := d.Store.SetDownstream(r.ID, string(r.Signal), r.At.Format(time.RFC3339)); err != nil {
			log.Printf("downstream record %d: %v", r.ID, err)
		}
	}
}

// seed records the requests that were already open when the bot first started as skipped, so it doesn't fire off
// a burst of reviews; they can be queued from the TUI.
func seed(d pipeline.Deps) error {
	prs, err := d.GH.RequestedFromMe(d.Cfg.Org)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if _, err := d.Store.Insert(&store.Review{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Title: pr.Title,
			Author: pr.Author, HeadSHA: pr.HeadSHA, Status: store.Ignored, Source: "auto",
			Decision: "requested before personal-review-bot started (press r in the TUI to review)"}); err != nil {
			return err
		}
	}
	log.Printf("first run: %d already-open request(s) recorded as skipped", len(prs))
	return d.Store.SetMeta("seeded", time.Now().UTC().Format(time.RFC3339))
}

func poll(d pipeline.Deps) {
	prs, err := d.GH.RequestedFromMe(d.Cfg.Org)
	if err != nil {
		log.Printf("poll: %v", err)
		return
	}
	_ = d.Store.SetMeta("last_poll", time.Now().UTC().Format(time.RFC3339))
	for _, pr := range prs {
		if (pr.Draft && d.Cfg.SkipDrafts) || d.Store.IsIgnored(pr.Repo, pr.Number) {
			continue
		}
		last, err := d.Store.Latest(pr.Repo, pr.Number)
		if err != nil {
			log.Printf("%s#%d: %v", pr.Repo, pr.Number, err)
			continue
		}
		if idle(pr, d.Cfg.MaxAge, time.Now()) {
			if last == nil { // record it once, so it shows in the TUI and can be reviewed with r
				days := int(d.Cfg.MaxAge.Hours() / 24)
				_, _ = d.Store.Insert(&store.Review{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Title: pr.Title,
					Author: pr.Author, HeadSHA: pr.HeadSHA, Status: store.Ignored, Source: idleSkip,
					Decision: fmt.Sprintf("no activity for %d days (press r in the TUI to review)", days)})
				log.Printf("%s#%d: skipped, no activity since %s", pr.Repo, pr.Number, lastActivity(pr).Format("2006-01-02"))
			}
			continue
		}
		source := reviewReason(pr, last)
		if source == "" {
			continue
		}
		if _, err := d.Store.Insert(&store.Review{Repo: pr.Repo, Number: pr.Number, URL: pr.URL, Title: pr.Title,
			Author: pr.Author, HeadSHA: pr.HeadSHA, Status: store.Queued, Source: source}); err != nil {
			log.Printf("queue %s#%d: %v", pr.Repo, pr.Number, err)
			continue
		}
		log.Printf("%s#%d: queued (%s, by @%s)", pr.Repo, pr.Number, source, pr.Author)
	}
}

// idle reports whether a PR has had no activity (commits, comments, reviews, edits) for longer than maxAge (0 = no
// limit), however long ago it was opened. Only automatic reviews check it; a PR queued by hand is always reviewed.
func idle(pr gh.PR, maxAge time.Duration, now time.Time) bool {
	last := lastActivity(pr)
	return maxAge > 0 && !last.IsZero() && now.Sub(last) > maxAge
}

// idleSkip is the source of the record left for a PR skipped as idle, so that activity on it brings it back.
const idleSkip = "idle"

func lastActivity(pr gh.PR) time.Time {
	if pr.UpdatedAt.IsZero() {
		return pr.CreatedAt
	}
	return pr.UpdatedAt
}

// newCommitsCooldown keeps a burst of pushes from triggering a review per push.
const newCommitsCooldown = 15 * time.Minute

// reviewReason says why a requested PR should be (re)reviewed now, or "" if it shouldn't. It is only asked about PRs
// that aren't idle.
func reviewReason(pr gh.PR, last *store.Review) string {
	switch {
	case last == nil:
		return "auto"
	case last.Status == store.Queued || last.Status == store.Running:
		return ""
	case !pr.RequestedAt.IsZero() && pr.RequestedAt.After(last.CreatedAt):
		return "re-request" // the author asked again, typically after answering or pushing fixes
	case pr.HeadSHA != last.HeadSHA && time.Since(last.CreatedAt) > newCommitsCooldown:
		return "new commits"
	case last.Status == store.Ignored && last.Source == idleSkip:
		return "activity" // skipped while nobody touched it; it has had activity since
	}
	return ""
}

// maxWorkers is how many workers the daemon starts at least: as many reviews at a time as the Settings page offers,
// so changing that there needs no restart.
const maxWorkers = 4

// worker runs queued reviews until ctx ends or an update is installed. Worker n only takes reviews while PARALLEL is
// above n, so the number running follows the setting. It counts a review as running from before it claims one, so
// the restart after an update can't slip in between the claim and the review.
func worker(ctx context.Context, d pipeline.Deps, upd *updater, n int) {
	for ctx.Err() == nil && !upd.draining() {
		cur := d.Current()
		var r *store.Review
		var err error
		upd.running.Add(1)
		if n < cur.Cfg.MaxParallel {
			if r, err = cur.Store.ClaimNext(); err != nil {
				log.Printf("queue: %v", err)
			}
		}
		if r == nil {
			upd.running.Add(-1)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err := pipeline.Run(ctx, cur, r.ID); err != nil {
			log.Printf("%s#%d: %v", r.Repo, r.Number, err)
		}
		upd.running.Add(-1)
	}
}
