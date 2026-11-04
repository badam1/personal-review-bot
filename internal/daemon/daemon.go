// Package daemon is the long-running bot: it polls GitHub for review requests addressed to the user directly,
// runs reviews on a small worker pool, and serves the confirmation page.
package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
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
	d := pipeline.Deps{Cfg: cfg, Store: st, GH: client, Log: log.Printf}
	log.Printf("personal-review-bot %s started for @%s (org %q, every %s, auto-post ≥ %d%% and %s+, page %s)",
		version.String(), client.Login, cfg.Org, cfg.PollInterval, cfg.Threshold, cfg.AutoSeverity, cfg.WebURL())

	if err := st.ResetRunning(); err != nil {
		return err
	}
	if st.Meta("seeded") == "" {
		if err := seed(d); err != nil {
			return fmt.Errorf("first run: %w", err)
		}
	}

	go func() {
		if err := web.Serve(ctx, d); err != nil {
			log.Printf("confirmation page stopped: %v", err)
		}
	}()
	for i := 0; i < cfg.MaxParallel; i++ {
		go worker(ctx, d)
	}

	tick := time.NewTicker(cfg.PollInterval)
	defer tick.Stop()
	for {
		poll(d)
		select {
		case <-ctx.Done():
			log.Printf("personal-review-bot stopping")
			return nil
		case <-tick.C:
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

func worker(ctx context.Context, d pipeline.Deps) {
	for ctx.Err() == nil {
		r, err := d.Store.ClaimNext()
		if err != nil {
			log.Printf("queue: %v", err)
		}
		if r == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err := pipeline.Run(ctx, d, r.ID); err != nil {
			log.Printf("%s#%d: %v", r.Repo, r.Number, err)
		}
	}
}
