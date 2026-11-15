# Implementation plan — portable, multi-channel personal-review-bot

Status: **draft** · 2026-11-15 · companion to [`portable-channels-scope.md`](./portable-channels-scope.md) (decisions D1–D5)

## Principles

- **Incremental.** Each phase is its own reviewable PR, green, with **no behavior change unless stated** — today's
  single-user laptop setup keeps working at every step (one implicit "default" channel synthesized from the current
  `config.env`).
- **The channel is the seam.** Everything per-deployment hangs off it; introduce it first, with zero functional
  change, then make each dimension pluggable behind it.
- **Secrets never in the config repo** — referenced by name/path, injected from env (D2/D3).

## Where each change lands (current code)

| Concern | Today | Touch-point |
| :- | :- | :- |
| Config | flat struct from `config.env` | `internal/config` — `Config`, `Load()`/`LoadFrom()` |
| Trigger | poll loop → requested-reviewer search | `internal/daemon` `Run()`; `internal/gh` `RequestedFromMe(orgs)` |
| Claude auth | ambient `claude` login | `internal/agents` `claude()` → `cmd.Env` ← **D2 injection point** |
| GitHub auth | user's `gh` login (go-gh), one global client | `internal/gh` `New()` ← **D3** |
| Web UI | served (fixed localhost addr) | `internal/web` `Serve()` ← **D4 base** |
| State | one SQLite file | `internal/store` `Open(dbPath)` ← **D5** |
| Review core | — | `internal/pipeline` `Deps{Cfg,Store,GH,Log}` |

## Phases (each ≈ one PR)

**Phase 0 — the channel seam (no behavior change).**
Add a `Channel` type (name; repo whitelist/blacklist; refs for Claude-auth, GitHub-identity, settings, surface) and
a loader that synthesizes **one default channel** from today's `config.env`. Thread a channel-scoped view through
the review path where per-channel values will later differ. *Done = identical behavior, tests green — pure refactor.*

**Phase 1 — config from a checked-out file set + matching (D1 config).**
Define the declarative channel-config file format (a file set in a config dir/repo), loaded alongside `config.env`
(which stays the fallback → the single default channel). Implement whitelist/blacklist **regex** matching for both
repo→channel and channel-name selection; add `--channels` / `CHANNELS` selection (default = all) with **fail-fast**
on missing prerequisites; validate that no repo is claimed by two channels.

**Phase 2 — pluggable Claude auth per channel (D2).**
Resolve a channel's Claude auth to subprocess env — `CLAUDE_CONFIG_DIR` | `CLAUDE_CODE_OAUTH_TOKEN` |
`ANTHROPIC_API_KEY` — and inject it at `agents.claude()`'s `cmd.Env`. Config references the mechanism + secret
source; the secret stays in env. Default (no override) = ambient login, so single-subscription users are unchanged.
*Now two channels can review with two subscriptions.*

**Phase 3 — multi-channel service loop (D1 realized in service mode). ← your scenario works here.**
Make the poll loop iterate the selected channels; each polls its own orgs (`RequestedFromMe` with the channel's
orgs + its config) and routes results to itself. One process now runs several channels — multi-org,
multi-subscription — on a laptop or server, with a single GitHub account.

**Phase 4 — pluggable GitHub identity per channel: token first (D3, part 1).**
Construct the `gh` client per channel with its identity; a token channel uses its own token instead of the ambient
login. *Optional for a single-GitHub-account user (one token covers all their orgs) — needed for the general case.*

**Phase 5 — pluggable state (D5) + a git data store (D6).**
Store backends: `none` (ephemeral/in-memory, done — `OpenMemory`), `local file` (today), `external DB`, and a **git
data repo** the bot auto-commits durable outcome records to (verbatim/downstream labels, cost, decision, reviewed
SHA — labels and numbers only, private repo, serialized pushes). The git store closes the ephemeral-analytics gap
(a CI run commits its record) and provides **idempotency** from its "reviewed `owner/repo#N` at `sha`" records —
which supersedes the fragile PR-derived last-SHA approach originally planned here (the bot posts as the user's own
account, and the watermark is not always in a review's body). Operational / in-flight state stays in SQLite.

**Phase 6 — on-demand mode + container (run anywhere).**
A one-shot entrypoint: given a PR ref (a GitHub Actions event, or a CLI arg), review it with the owning channel,
post, exit; `state = none`. Ship a **container image** (the common unit for server / k8s / Actions); keep the
binary. *Enables GitHub Actions with no daemon and no volume.*

**Phase 7 — GitHub App identity + webhook receiver (D3 part 2 + webhook trigger).**
GitHub App auth (per-install short-lived tokens); a webhook endpoint that verifies the signature, maps
installation/repo → channel (edge-routed), and runs the one-shot review. *Native event-driven multi-org; the
community-install path.*

**Phase 8 — surface: web bind/auth + outbound notifier (D4).**
Make the web UI's bind address + auth token configurable (serveable beyond localhost); add a per-channel outbound
notifier (Slack incoming-webhook / bot token, or a generic webhook) that pings with a link. Interactive Slack
handling stays a later extension behind the same surface interface.

## Explicitly later / out of scope (per the cut)

- Interactive Slack handling (a Slack app + a verified callback endpoint).
- Helm chart, GitHub Actions workflow templates, server recipes — the deployment/hosting mechanics.
- A shared multi-writer DB (Postgres) store.

## Sequencing & milestones

- **Milestone A (your scenario):** after **Phase 3** — company + personal channels, multi-org, two subscriptions,
  one process.
- **Milestone B (run anywhere):** after **Phase 6** — stateless one-shot in a container / Action.
- **Milestone C (webhook / community):** after **Phase 7** — App-installed, event-driven, installable by others.
- Phases **4, 5, 8** are fairly independent and can reorder around 6/7.

## Verify during build (don't assume)

- `CLAUDE_CONFIG_DIR` keychain-scoping on the exact deployed Claude Code version (verified for 2.1.x).
- How go-gh takes a **specific** token / transport per channel rather than the ambient `gh` login (`gh.New()`
  currently builds one global client).
- Webhook signature verification and GitHub App JWT → installation-token minting (standard, but new surface).
- The store interface must preserve current SQLite behavior and migrations.
