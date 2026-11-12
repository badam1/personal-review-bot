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
	log.Printf("personal-review-bot %s started for @%s (orgs %s, every %s, auto-post ≥ %d%% for %s, page %s)",
		version.String(), client.Login, cfg.OrgsLabel(), cfg.PollInterval, cfg.Threshold, auto, cfg.WebURL())

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
	go watchPRs(ctx, d)

	tick := time.NewTicker(pollCheckEvery)
	defer tick.Stop()
	var next time.Time
	for {
		cur := d.Current()
		// Round(0) drops the monotonic reading, so the times compare by the wall clock, which runs on while asleep.
		if now := time.Now().Round(0); pollDue(now, next, cur.Cfg.PollInterval) {
			next = now.Add(cur.Cfg.PollInterval)
			if !poll(cur) {
				next = now.Add(min(pollRetry, cur.Cfg.PollInterval))
			}
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

// When to poll for review requests. Go's timers on a Mac don't count the time it sleeps, so a ticker of the poll
// interval came up to an interval late after a wake, the bar saying "Not watching" meanwhile. Instead the loop looks
// often and polls once the wall clock says one is due. A poll that failed is tried again sooner.
const (
	pollCheckEvery = 10 * time.Second
	pollRetry      = 30 * time.Second
)

// pollDue says whether a poll planned for next is due at now: once its time has come, and at once when it is further
// off than a whole interval (the clock was set back, or the interval shortened on the Settings page).
func pollDue(now, next time.Time, interval time.Duration) bool {
	return !now.Before(next) || next.Sub(now) > interval
}

// How the state of the PRs on the page is kept current: often for those waiting for you, since one merged or closed
// meanwhile leaves "Needs you", and now and then for the rest of the last month's, for the lists' icons. A merged or
// closed PR isn't asked about again.
const (
	prCheckEvery   = time.Minute
	prWaitingEvery = 10 * time.Minute
	prOtherEvery   = time.Hour
	prRecent       = 30 * 24 * time.Hour
	prBatch        = 10 // GitHub calls per check, so a first run over a long history spreads over a few minutes
)

// watchPRs keeps the PRs' states current until ctx ends. It runs apart from the poll, so a slow GitHub can't hold
// up new review requests.
func watchPRs(ctx context.Context, d pipeline.Deps) {
	tick := time.NewTicker(prCheckEvery)
	defer tick.Stop()
	for {
		checkPRs(d.Current(), d.GH.GetPR, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// checkPRs asks GitHub for the state of the PRs due a check and records it. A PR merged or closed while a review of
// it waited for you leaves "Needs you".
func checkPRs(d pipeline.Deps, get func(repo string, number int) (gh.PR, error), now time.Time) {
	due, err := d.Store.PRsToCheck(now.Add(-prWaitingEvery), now.Add(-prOtherEvery), now.Add(-prRecent), prBatch)
	if err != nil {
		log.Printf("PR states: %v", err)
		return
	}
	for _, k := range due {
		pr, err := get(k.Repo, k.Number)
		if err != nil {
			log.Printf("%s#%d: couldn't read the PR's state: %v", k.Repo, k.Number, err)
			_ = d.Store.TouchPR(k.Repo, k.Number)
			continue
		}
		st := pipeline.PRStatus(pr)
		if err := d.Store.SetPRStatus(k.Repo, k.Number, st); err != nil {
			log.Printf("%s#%d: %v", k.Repo, k.Number, err)
			continue
		}
		if n, err := pipeline.CloseOutIfOver(d.Store, k.Repo, k.Number, st); err != nil {
			log.Printf("%s#%d: %v", k.Repo, k.Number, err)
		} else if n > 0 {
			log.Printf("%s#%d: %s before you decided, so it no longer needs you", k.Repo, k.Number, st.State)
		}
	}
}

// seed records the requests that were already open when the bot first started as skipped, so it doesn't fire off
// a burst of reviews; they can be queued from the TUI.
// requested lists the open review requests addressed to you, and says so when GitHub matched more than one
// search returns: the ones left out are never reviewed, and watching several orgs makes that likelier.
func requested(d pipeline.Deps) ([]gh.PR, error) {
	prs, total, err := d.GH.RequestedFromMe(d.Cfg.Orgs)
	if err != nil {
		return nil, err
	}
	if total > len(prs) {
		log.Printf("GitHub matched %d open review requests for you but one search returns %d: %d are not being looked at. Narrow ORG.",
			total, len(prs), total-len(prs))
	}
	return prs, nil
}

func seed(d pipeline.Deps) error {
	prs, err := requested(d)
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

// poll queues the PRs that need a review, and says whether GitHub answered.
func poll(d pipeline.Deps) bool {
	prs, err := requested(d)
	if err != nil {
		log.Printf("poll: %v", err)
		return false
	}
	_ = d.Store.SetMeta("last_poll", time.Now().UTC().Format(time.RFC3339))
	for _, pr := range prs {
		_ = d.Store.SetPRStatus(pr.Repo, pr.Number, pipeline.PRStatus(pr)) // open or draft: the search finds only those
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
	return true
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
