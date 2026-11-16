package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/version"
	"github.com/badam1/personal-review-bot/internal/webhook"
)

// ServeWebhook runs the on-demand path: an HTTP endpoint that reviews a PR when GitHub delivers a pull_request event,
// under the channel that owns the repo, keeping no state on disk. It is the community-install counterpart to the
// poller (Run), and the two share reviewDeps. The webhook secret comes from GITHUB_WEBHOOK_SECRET.
//
// A pure App deployment has no ambient `gh` login, so a missing one is not fatal here: a repo that routes to a channel
// with its own identity (App or token) still reviews; one that would fall back to the ambient login is skipped with a
// clear log rather than crashing.
func ServeWebhook(ctx context.Context, cfg config.Config) error {
	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		return fmt.Errorf("GITHUB_WEBHOOK_SECRET is not set: refusing to accept unsigned webhooks")
	}
	client, err := gh.New()
	if err != nil {
		log.Printf("no ambient GitHub login (%v); reviews must route to a channel with its own identity", err)
		client = nil
	}
	base, err := reviewDeps(ctx, cfg, client)
	if err != nil {
		return err
	}

	// Bound how many reviews run at once, as the poller's worker pool does; extra deliveries queue (GitHub already got
	// its 202).
	sem := make(chan struct{}, max(1, cfg.MaxParallel))
	handler := &webhook.Handler{
		Secret: secret,
		Base:   ctx,
		Log:    log.Printf,
		Review: func(rctx context.Context, ev webhook.Event) {
			channel, owned := base.RouteChannel(ev.Repo)
			if !owned {
				log.Printf("webhook: %s#%d: no channel owns it, ignoring", ev.Repo, ev.Number)
				return
			}
			select {
			case sem <- struct{}{}:
			case <-rctx.Done():
				return
			}
			defer func() { <-sem }()
			reviewOnce(rctx, base, ev, channel)
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	addr := cfg.WebhookAddr
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	log.Printf("personal-review-bot %s webhook server on %s", version.String(), addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// reviewOnce reviews one PR from a webhook event in an in-memory store, leaving nothing on disk. It runs under the
// owning channel's identity; the data store (when configured) gives idempotency against duplicate deliveries and the
// last reviewed commit for an incremental re-review.
func reviewOnce(ctx context.Context, base pipeline.Deps, ev webhook.Event, channel string) {
	rv := &store.Review{Repo: ev.Repo, Number: ev.Number, HeadSHA: ev.HeadSHA,
		Status: store.Running, Source: "auto", Channel: channel}
	if base.ClientFor(rv) == nil {
		log.Printf("webhook: %s#%d: channel %q has no GitHub identity (no App or token, and no ambient login), skipping",
			ev.Repo, ev.Number, channelLabel(channel))
		return
	}
	st, err := store.OpenMemory()
	if err != nil {
		log.Printf("webhook: %s#%d: store: %v", ev.Repo, ev.Number, err)
		return
	}
	defer st.Close()
	d := base
	d.Store = st
	id, err := st.Insert(rv)
	if err != nil {
		log.Printf("webhook: %s#%d: %v", ev.Repo, ev.Number, err)
		return
	}
	log.Printf("webhook: reviewing %s#%d (%s) on channel %q", ev.Repo, ev.Number, ev.Action, channelLabel(channel))
	if err := pipeline.Run(ctx, d, id); err != nil {
		log.Printf("webhook: %s#%d: %v", ev.Repo, ev.Number, err)
	}
}

func channelLabel(name string) string {
	if name == "" {
		return "default"
	}
	return name
}
