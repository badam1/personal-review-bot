# Tasks — portable, multi-channel personal-review-bot

Status: **open** · 2026-11-15 · derived from [`portable-channels-plan.md`](./portable-channels-plan.md) and the
decisions in [`portable-channels-scope.md`](./portable-channels-scope.md). Tracked here (not in an external backlog)
so the work is versioned with the project.

**Legend:** `[ ]` to do · `[~]` in progress · `[x]` done. Each task gives its **Outcome** (how you know it's done)
and what it **Depends on**. IDs (`P0.1`) are stable references. Each phase is intended to land as its own PR, green,
with no behavior change unless the task says otherwise.

---

## Milestone A — the "company + personal, multi-channel" scenario (service mode)

### P0 — Channel seam (no behavior change)
- [ ] **P0.1 — `Channel` type + default-channel loader.** Add a `Channel` (name; repo whitelist/blacklist;
  Claude-auth ref; GitHub-identity ref; settings; surface) and a loader that synthesizes **one default channel**
  from today's `config.env`.
  - Outcome: `config.Load` still works; the default channel represents current settings; loader unit-tested.
  - Depends on: —
- [ ] **P0.2 — Thread a channel-scoped view through the review + poll path** in place of the global `Cfg` where
  per-channel values will later differ.
  - Outcome: behavior identical to today; full suite green; no new config required to run.
  - Depends on: P0.1

### P1 — Channel config file + matching (D1)
- [ ] **P1.1 — Declarative channel-config file.** Parse channels from a file set in a config dir/repo; absent file
  falls back to `config.env` (the single default channel).
  - Outcome: N channels definable in a file; no file → today's single-channel behavior.
  - Depends on: P0.1
- [ ] **P1.2 — Whitelist/blacklist regex matching.** Repo→channel and channel-name matching; reject a repo claimed
  by two channels at load.
  - Outcome: a repo resolves to exactly one owning channel; overlap is a clear config error.
  - Depends on: P1.1
- [ ] **P1.3 — Channel selection (`--channels` / `CHANNELS`, default all) + fail-fast.** An instance runs the named
  subset; missing prerequisites for a selected channel error at boot.
  - Outcome: an instance runs exactly its channels, or exits with a clear message.
  - Depends on: P1.2

### P2 — Per-channel Claude auth (D2)
- [ ] **P2.1 — Inject Claude auth per channel at `agents.claude()`.** Resolve a channel's auth to env
  (`CLAUDE_CONFIG_DIR` | `CLAUDE_CODE_OAUTH_TOKEN` | `ANTHROPIC_API_KEY`) and set it on the subprocess.
  - Outcome: a channel reviews under its declared subscription; no override = ambient login (unchanged); secret read
    from env, never from the config repo.
  - Depends on: P0.2

### P3 — Multi-channel service loop  ⭐ Milestone A
- [ ] **P3.1 — Poll loop iterates selected channels.** Each polls its own orgs (`RequestedFromMe`) with its config
  and routes results to itself.
  - Outcome: one process runs ≥2 channels with different subscriptions/orgs; the company + personal scenario works
    in service mode with a single GitHub account.
  - Depends on: P1.3, P2.1
- [ ] **P3.2 — Attribute reviews to their channel in the UI** (minimal).
  - Outcome: each review shows which channel handled it.
  - Depends on: P3.1

---

## Milestone B — run anywhere (on-demand / container)

### P4 — Per-channel GitHub identity: token (D3 part 1)
- [x] **P4.1 — Build the `gh` client per channel with its token** (vs. the ambient `gh` login).
  - Outcome: a channel can act under a distinct token; default = ambient login. `gh.NewWithToken` pins the host and
    consults no gh config (works in CI); `Deps.ClientFor` resolves a review's client. *(Optional for a single-GitHub-
    account user — one token already covers all their orgs.)* ✅ committed (PR #52).
  - Depends on: P0.2

### P5 — Pluggable state (D5) + git data store (D6)
- [x] **P5.1 — Ephemeral store for one-shot runs.** `store.OpenMemory` opens the schema in memory; `local file` and
  `external/volume` are the same SQLite at a different path, so no store *interface* was needed.
  - Outcome: a one-shot run keeps state in memory and leaves nothing on disk; file store unchanged. ✅ committed.
  - Depends on: —
- [x] **P5.3 — Git data store (D6).** A store backend that auto-commits durable outcome records (verbatim/downstream
  labels, cost, decision, reviewed SHA — labels and numbers only) to a **private** git data repo, Backlog.md-style;
  the bot only commits — pushing is the data repo's own post-commit hook, as Backlog.md does — so operational state
  stays in SQLite and no unverified push code ships.
  - Outcome: analytics are durable, versioned, and shareable with no DB server; a CI run commits its record so
    analytics accumulate even from ephemeral compute; the "reviewed `owner/repo#N` at `sha`" records give idempotency
    (`skipReview` skips an automatic re-run of a recorded head). ✅ committed (PR #51).
  - Depends on: P5.1
- [x] **P5.2 — Incremental re-review (idempotency via D6).** Know "already reviewed this SHA?" and "last reviewed
  commit" without local state. **D6 supersedes the hard part**: take these from the git data store's records rather
  than deriving them off the PR (fragile — the bot posts as the user's own account, and the watermark is not always in
  a review's body). Pure CI idempotency can also be handled at deploy time (Actions `concurrency`), so this narrows to
  *incremental* re-review (a cost optimization).
  - Outcome: a re-review covers only what changed, with no local state — the pipeline fills `PrevSHA` from
    `datarepo.LastReviewed` when SQLite has no history. ✅ committed (PR #51).
  - Depends on: P5.3

### P6 — On-demand mode + container  ⭐ Milestone B
- [x] **P6.1 — One-shot entrypoint.** `review --ephemeral <PR-URL>` reviews one PR in an in-memory store and exits.
  - Outcome: a single run reviews once and exits cleanly, no daemon, no volume. ✅ committed.
  - Depends on: P1.2, P2.1, P5.1
- [ ] **P6.2 — Container image.** Keep the binary.
  - Outcome: an image runs the bot unattended with all creds from env; a published build.
  - Depends on: P6.1

---

## Milestone C — webhook / community install

### P7 — GitHub App identity + webhook receiver (D3 part 2)
- [x] **P7.1 — GitHub App auth.** App JWT → per-install short-lived token in the `gh` client.
  - Outcome: the bot acts as the App with per-install tokens; nothing long-lived on the server but the App key.
    `internal/ghapp` signs the JWT, mints + caches installation tokens, and wraps gh's transport; `gh.NewWithAuth`
    builds the App client. ✅ committed (PR #52). *Live JWT acceptance + real token mint need a registered App —
    built to GitHub's contract, tested behind seams.*
  - Depends on: P4.1
- [x] **P7.2 — Webhook receiver.** Verify the signature, map installation/repo → channel (edge-routed), run the
  one-shot review.
  - Outcome: a PR event triggers a review with the owning channel; bad signatures are rejected. `internal/webhook`
    (HMAC verify + parse + dispatch) and the `webhook` command (`daemon.ServeWebhook`) review in memory under the
    channel's identity. ✅ committed (PR #52). *An end-to-end delivery needs a registered App — the verify/parse/route
    are tested.*
  - Depends on: P6.1, P7.1

---

## Surface (independent; can land anytime after P3)

### P8 — Web remote access + outbound notifier (D4)
- [ ] **P8.1 — Configurable web bind address + auth token.**
  - Outcome: the UI can be served beyond localhost behind a token.
  - Depends on: —
- [ ] **P8.2 — Per-channel outbound notifier** (Slack incoming-webhook / bot token, or a generic webhook) with a link.
  - Outcome: a review needing attention pings the channel's surface.
  - Depends on: P3.1

---

## Parked (out of scope for this effort)

- [ ] Interactive Slack handling — a Slack app + a verified callback endpoint.
- [ ] Helm chart / GitHub Actions workflow templates / server recipes (deployment mechanics).
- [ ] Shared multi-writer store (Postgres).

## Verify during build (don't assume)

- `CLAUDE_CONFIG_DIR` keychain-scoping on the exact deployed Claude Code version (verified for 2.1.x).
- How go-gh accepts a specific token/transport per channel rather than the ambient `gh` login.
- Webhook signature verification + App JWT → installation-token minting.
- The store interface must preserve current SQLite behavior and migrations.
