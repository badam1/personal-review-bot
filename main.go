// personal-review-bot reviews PRs where a review is requested from you directly (not via a team): Claude reviewers sized
// to the PR, a verifier that scores every finding, auto-posting only at ≥ threshold, and a local page for the rest.
// It never approves on its own.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/daemon"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/tui"
	"github.com/badam1/personal-review-bot/internal/update"
	"github.com/badam1/personal-review-bot/internal/version"
	"github.com/badam1/personal-review-bot/internal/web"
)

const usage = `personal-review-bot: reviews PRs where your review is requested directly

  personal-review-bot                       terminal UI: what's running and the last 10 reviews
  personal-review-bot status                the same, printed once
  personal-review-bot daemon                run the bot (launchd runs this)
  personal-review-bot review [--dry-run] [--deep] <PR-URL>
                                   review one PR now; --dry-run posts nothing and opens nothing;
                                   --deep reviews it as if it were large (Opus, the design review)
  personal-review-bot queue [--deep] <PR-URL>
                                            hand a PR to the running daemon
  personal-review-bot page                  serve only the confirmation page (no polling, no reviews)
  personal-review-bot agents                the reviewers and prompts reviews run with, and where each comes from
  personal-review-bot agents export <dir>   write the built-in reviewers, prompts and an example context.md to <dir>
  personal-review-bot version               the version of this build
  personal-review-bot update [--check]      install the newest release (through the daemon when it runs);
                                            --check only says whether there is one
`

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "personal-review-bot:", err)
		os.Exit(2)
	}
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
		err = runDaemon(ctx, cfg)
	case "update":
		err = updateCmd(ctx, cfg, args)
	case "review":
		err = review(ctx, cfg, args)
	case "queue":
		err = queue(cfg, args)
	case "page":
		err = page(ctx, cfg)
	case "agents":
		err = agentsCmd(cfg, args, os.Stdout)
	case "version", "--version":
		fmt.Println("personal-review-bot", version.String())
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
func newReview(st *store.Store, client *gh.Client, url, status string, dry, deep bool) (int64, error) {
	repo, num, err := gh.ParseURL(url)
	if err != nil {
		return 0, err
	}
	pr, err := client.GetPR(repo, num)
	if err != nil {
		return 0, err
	}
	return st.Insert(&store.Review{Repo: repo, Number: num, URL: pr.URL, Title: pr.Title, Author: pr.Author,
		HeadSHA: pr.HeadSHA, Status: status, Source: "manual", DryRun: dry, Deep: deep})
}

func review(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "post nothing, open nothing")
	deep := fs.Bool("deep", false, "review it as if it were large: Opus, the design review, no usage savings")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: personal-review-bot review [--dry-run] [--deep] <PR-URL>")
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
	id, err := newReview(st, client, fs.Arg(0), store.Running, *dry, *deep)
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
	return web.Serve(ctx, pipeline.Deps{Cfg: cfg, Store: st, GH: client, Log: log.Printf}, nil)
}

func queue(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("queue", flag.ExitOnError)
	deep := fs.Bool("deep", false, "review it as if it were large: Opus, the design review, no usage savings")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: personal-review-bot queue [--deep] <PR-URL>")
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
	id, err := newReview(st, client, fs.Arg(0), store.Queued, false, *deep)
	if err != nil {
		return err
	}
	fmt.Printf("queued as review %d; the daemon picks it up within a few seconds\n", id)
	return nil
}

// agentsCmd shows the reviewers and prompts a review would run with (checking the config directory's files on the
// way), or exports the built-in ones to copy from.
func agentsCmd(cfg config.Config, args []string, w io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "export":
		wrote, err := agents.Export(args[1])
		for _, path := range wrote {
			fmt.Fprintln(w, "wrote", path)
		}
		if err != nil {
			return err
		}
		if len(wrote) == 0 {
			fmt.Fprintln(w, "nothing written: every file is already there")
		}
		fmt.Fprintf(w, "Copy the files you want to change into %s, in the same folders.\n", cfg.ConfigDir)
		return nil
	case len(args) > 0:
		return fmt.Errorf("usage: personal-review-bot agents [export <dir>]")
	}
	k, err := agents.Load(cfg.ConfigDir)
	if err != nil {
		return err
	}
	rel := func(source string) string {
		if r, err := filepath.Rel(cfg.ConfigDir, source); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return source
	}
	fmt.Fprintf(w, "Config directory: %s\n\nReviewers\n", cfg.ConfigDir)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range k.Reviewers {
		runs := "every PR"
		if r.Paths != nil {
			runs = "paths " + r.Paths.String()
			if len(runs) > 48 {
				runs = runs[:47] + "…"
			}
		}
		if r.Docs {
			runs += ", docs-only too"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", r.Name, r.Area, runs, cmp.Or(r.Model, "tier's model"), rel(r.Source))
	}
	_ = tw.Flush()
	for _, r := range k.Disabled {
		fmt.Fprintf(w, "  switched off: %s (%s)\n", r.Name, rel(r.Source))
	}
	fmt.Fprintln(w, "\nPrompts")
	for _, name := range agents.Prompts {
		fmt.Fprintf(tw, "  %s\t%s\n", name, rel(k.Sources[name]))
	}
	_ = tw.Flush()
	if k.Context != "" {
		fmt.Fprintf(w, "\nContext: %s (%d lines), added to every prompt\n", rel(k.Sources["context"]), strings.Count(k.Context, "\n")+1)
	} else {
		fmt.Fprintln(w, "\nContext: none (add context.md to tell every prompt about your organisation)")
	}
	return nil
}

// runDaemon runs the bot. After an update it restarts into the new binary in place, as the same process, so launchd,
// systemd or a terminal keep it without noticing.
func runDaemon(ctx context.Context, cfg config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := daemon.Run(ctx, cfg); !errors.Is(err, daemon.ErrRestart) {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// updateCmd installs the newest release: through the running daemon, which restarts into it once its reviews finish,
// or, when no daemon runs, by replacing this binary.
func updateCmd(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only say whether a newer release is out")
	_ = fs.Parse(args)
	client, err := gh.New()
	if err != nil {
		return err
	}
	releases, err := client.Releases(cfg.UpdateRepo)
	if err != nil {
		return fmt.Errorf("releases of %s: %w", cfg.UpdateRepo, err)
	}
	rel, ok := update.Newest(releases, version.String(), runtime.GOOS, runtime.GOARCH)
	if !ok {
		fmt.Printf("personal-review-bot %s is up to date\n", version.String())
		return nil
	}
	if *check {
		fmt.Printf("%s is available (running %s): %s\n", rel.Version, version.String(), rel.URL)
		return nil
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	msg, err := update.Request(cfg, st)
	switch {
	case err == nil:
		fmt.Println(msg)
		return followUpdate(ctx, st, rel)
	case !errors.Is(err, update.ErrNoDaemon):
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Printf("no daemon running: installing %s in %s\n", rel.Version, exe)
	step := func(s string) { fmt.Println("  " + s) }
	if err := update.Install(ctx, client, cfg.UpdateRepo, rel, exe, update.Attested, step); err != nil {
		return err
	}
	fmt.Printf("✓ updated to %s (%s)\n", rel.Version, rel.URL)
	return nil
}

// followUpdate prints the daemon's update as it goes, until the daemon runs rel, the update fails, or 15 minutes pass
// (it may be waiting for long reviews; it restarts on its own).
func followUpdate(ctx context.Context, st *store.Store, rel update.Release) error {
	deadline := time.Now().Add(15 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		if update.DaemonVersion(st) == rel.Version {
			fmt.Printf("✓ updated: the daemon runs %s (%s)\n", rel.Version, rel.URL)
			return nil
		}
		state := update.State(st)
		if state != last && state != "" {
			fmt.Println("  " + state)
			last = state
		}
		if strings.Contains(state, "failed") {
			return errors.New(state)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	fmt.Println("still going after 15 minutes; the daemon restarts on its own once its running reviews finish")
	return nil
}
