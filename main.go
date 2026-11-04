// personal-review-bot reviews PRs where a review is requested from you directly (not via a team): Claude reviewers sized
// to the PR, a verifier that scores every finding, auto-posting only at ≥ threshold, and a local page for the rest.
// It never approves on its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/daemon"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/tui"
	"github.com/badam1/personal-review-bot/internal/web"
)

const usage = `personal-review-bot: reviews PRs where your review is requested directly

  personal-review-bot                       terminal UI: what's running and the last 10 reviews
  personal-review-bot status                the same, printed once
  personal-review-bot daemon                run the bot (launchd runs this)
  personal-review-bot review [--dry-run] <PR-URL>
                                   review one PR now; --dry-run posts nothing and opens nothing
  personal-review-bot queue <PR-URL>        hand a PR to the running daemon
  personal-review-bot page                  serve only the confirmation page (no polling, no reviews)
`

func main() {
	cfg := config.Load()
	cmd, args := "tui", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "tui":
		err = tui.Run(cfg)
	case "status":
		err = tui.PrintStatus(cfg, os.Stdout)
	case "daemon":
		err = daemon.Run(ctx, cfg)
	case "review":
		err = review(ctx, cfg, args)
	case "queue":
		err = queue(cfg, args)
	case "page":
		err = page(ctx, cfg)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "personal-review-bot:", err)
		os.Exit(1)
	}
}

// newReview records a PR in the store with the given status.
func newReview(st *store.Store, client *gh.Client, url, status string, dry bool) (int64, error) {
	repo, num, err := gh.ParseURL(url)
	if err != nil {
		return 0, err
	}
	pr, err := client.GetPR(repo, num)
	if err != nil {
		return 0, err
	}
	return st.Insert(&store.Review{Repo: repo, Number: num, URL: pr.URL, Title: pr.Title, Author: pr.Author,
		HeadSHA: pr.HeadSHA, Status: status, Source: "manual", DryRun: dry})
}

func review(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "post nothing, open nothing")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: personal-review-bot review [--dry-run] <PR-URL>")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	client, err := gh.New()
	if err != nil {
		return err
	}
	id, err := newReview(st, client, fs.Arg(0), store.Running, *dry)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.Ltime)
	if err := pipeline.Run(ctx, pipeline.Deps{Cfg: cfg, Store: st, GH: client, Log: logger.Printf}, id); err != nil {
		return err
	}
	r, err := st.Get(id)
	if err != nil {
		return err
	}
	findings, err := st.Findings(id)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s#%d %q by @%s\ntier %s (%s) · $%.2f · %s\n", r.Repo, r.Number, r.Title, r.Author, r.Tier, r.Models, r.CostUSD, r.Decision)
	if r.Summary != "" {
		fmt.Printf("summary: %s\n", r.Summary)
	}
	if r.Error != "" {
		fmt.Printf("errors: %s\n", r.Error)
	}
	for _, f := range findings {
		fmt.Printf("\n[%s] %d%% %s · %s:%d · %s (%s, %s)\n  %s\n", f.Status, f.Confidence, f.Verdict, f.Path, f.Line,
			f.Title, f.Agent, f.Severity, f.Comment)
	}
	fmt.Printf("\nreview page: %s/r/%d (needs the daemon running)\n", cfg.WebURL(), id)
	return nil
}

// page serves the confirmation page without the daemon (decisions on waiting reviews still work).
func page(ctx context.Context, cfg config.Config) error {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	client, err := gh.New()
	if err != nil {
		return err
	}
	fmt.Printf("confirmation page on %s (Ctrl+C to stop)\n", cfg.WebURL())
	return web.Serve(ctx, pipeline.Deps{Cfg: cfg, Store: st, GH: client, Log: log.Printf})
}

func queue(cfg config.Config, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: personal-review-bot queue <PR-URL>")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	client, err := gh.New()
	if err != nil {
		return err
	}
	id, err := newReview(st, client, args[0], store.Queued, false)
	if err != nil {
		return err
	}
	fmt.Printf("queued as review %d; the daemon picks it up within a few seconds\n", id)
	return nil
}
