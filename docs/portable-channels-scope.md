# Running personal-review-bot anywhere — portable, multi-channel (scope note)

Status: **draft for discussion — core design resolved (D1–D5)** · 2026-11-15

## Why

Today the bot is a laptop app: launchd starts it, a fixed local SQLite file holds state,
`claude` and `gh` are assumed logged in on the same machine, and the web UI is bound to
localhost. As a community tool, people need to run it in very different places — their own
laptop, a GitHub Actions runner, a dedicated server, or Kubernetes via a Helm chart — and
with different setups: several GitHub orgs, and sometimes more than one Claude subscription.

This note scopes what the **bot itself** needs so it can run anywhere, flexibly.

## Scope / non-goals

- **In scope:** the bot-side contract — run modes, the "channel" config unit, routing,
  config-vs-state, and packaging.
- **Out of scope (deliberately):** how and where it is actually hosted — the server /
  runner / cluster setup, networking, secret delivery, CI wiring. Those differ per
  deployment and come *after* the bot supports them.

## Core idea: separate the brain from the environment

The review core — decide what to review → run Claude → post → track — is sound. What is
fused to the laptop is everything *around* it. "Encapsulate" means giving the core a clean,
injectable contract for each of those, and shipping it as one unit: a **container image**
(the common denominator for server / k8s / Actions), with the plain binary kept for local.

## Two run modes (same core)

- **Service mode (today):** long-running; polls GitHub for the repos it owns; keeps state;
  serves a UI. Fits laptop / server / k8s.
- **On-demand mode (new):** a GitHub Actions job or a webhook fires on a PR; the bot reviews
  that one PR, posts, and exits. Little or no persistent state. Fits Actions / event-driven
  servers.

Most other choices (state, UI, how config arrives) follow from which mode is in use.

## The channel: the unit of configuration

A **channel** bundles what currently cannot vary:

```
channel { orgs/repos[],  claude-subscription,  settings,  surface }
```

- A channel **owns a set of orgs/repos** and reviews them all with **one** Claude
  subscription (plus one settings set and one surface — e.g. the local web UI, or Slack).
- **Ownership is expressed as whitelist + blacklist patterns (regex),** not fixed lists — a
  channel claims e.g. `acme/.*` minus `acme/archived-.*`. A plain `owner/repo` is just a
  trivial pattern. This keeps config terse, and it is the *same* matching used to select
  channels (see Decisions).
- The **subscription is the 1:1 axis**: it is what defines where one channel ends and the
  next begins. Orgs/repos fan out *underneath* it.
- You split into a new channel when the subscription — or the settings, or the surface —
  must differ.

## Routing: which channel handles an event

- Route on the **repo's org (owner)** — the one signal always present on an event, and how
  people actually think ("company orgs" vs "mine"). Repo- or label-level rules refine it for
  edge cases.
- **Rule:** a repo belongs to **exactly one** channel. Claims are ordered whitelist/blacklist
  patterns; the first channel whose whitelist matches and whose blacklist does not wins, and
  config flags a repo claimed by two channels.
- "Where it physically goes" is a deployment choice over the *same* config:
  - **Edge routing (simplest, most isolated):** point each org's webhook / GitHub App
    installation at the instance that owns it. No central router — the destination *is* the
    address you configured, and each instance only ever sees its own events.
  - **Single front door:** one endpoint with an `org → channel` map dispatches. One place to
    configure, but one component to keep reachable and secured.
- Same ownership model as service mode: a channel *polls* the repos it owns, or *receives*
  events for them.

## Config vs. state

- **Config** (channels, settings) → declarative files in a **checked-out git repo**:
  versioned, reviewable, and portable — the *same* repo drives the bot on a laptop, a server,
  k8s (GitOps), or a GitHub Action (the config is right there in the checkout). Same ethos as
  Backlog.md: plain files in git, no hidden database.
- **State** (reviews in flight, findings, downstream signals) → stays in the **store**
  (a SQLite file / volume). It churns on every poll and is racy across instances, so it is
  not a fit for committed files. In on-demand mode, most "state" is just the comments already
  on the PR, so there is little to keep.

## Packaging

- **Container image** — the common unit for server, k8s (Helm), and Actions.
- **Plain binary** — kept for local use.
- **Credentials** (GitHub, Claude) injected as secrets / env, never interactive login, so the
  same image runs unattended anywhere.

## Decisions

**D1 — Which channels an instance runs: explicit, by name, default = all.**
An instance is told which channels to run (`--channels company`, or `CHANNELS=company,personal`);
omitted, it runs every channel in the config. The config repo stays the single source of truth
listing *all* channels; each deployment names its slice — so "company on the laptop / personal
anywhere" needs no second config. Naming one channel gives a per-channel instance; naming several
(or omitting) gives one multiplexing process — same binary, same config, the deployment decides.
An instance **fails fast at boot** if a selected channel's subscription/creds are missing, and the
selection **scopes the trigger** (polls only its channels' orgs; accepts webhook events only for
their repos), which also keeps each channel to one instance.

Channel selection and repo ownership both use **whitelist + blacklist patterns (regex)**, so
configuring either is as terse as possible — `--channels 'team-.*'` runs a family of channels, a
channel claims `acme/.*` minus `acme/archived-.*`, and a plain name/`owner/repo` is a trivial
pattern.

**D2 — Switching the Claude subscription per channel: inject auth into the `claude` subprocess, by environment.**
The bot already runs `claude -p` as a subprocess and controls its environment, so a channel's auth is just env
it sets per review. A channel declares *which* mechanism and *which* secret/dir it uses; the secret itself lives
in the instance's env / secret store, **never in the config repo**. Env credentials outrank the ambient `/login`,
so injecting one deterministically selects that channel's account. Default (no override) = the ambient login, so
a single-subscription user needs zero auth config.

Three mechanisms, all env-injectable (verified against the Claude Code authentication docs, 2.1.x):

- **`CLAUDE_CONFIG_DIR=<dir>`** — a dedicated config directory per subscription, each with its own login. It
  *scopes the macOS Keychain entry to the directory* (not one shared item), so two subscriptions don't collide;
  on Linux/containers each dir keeps its own `.credentials.json`. Best where you can log in once (a laptop) or
  provision the directory.
- **`CLAUDE_CODE_OAUTH_TOKEN=<token>`** — a long-lived (1-year) **subscription** token from `claude setup-token`;
  no interactive login and no keychain, just a secret. The right mechanism for **servers / containers / CI** (it
  makes model requests only, which is all the reviewer needs).
- **`ANTHROPIC_API_KEY=<key>`** — Console / API-key billing, for a channel that prefers that over a subscription.

Extendable to more auth types (Bedrock/Vertex, `apiKeyHelper`, gateway) through the same env seam, without
changing the channel model.

**D3 — GitHub identity: pluggable per channel — a GitHub App for webhook/server/community, a user token for local.**
Same shape as D2: a channel declares `github: { app: … }` or `github: { token: … }`, with the secret in the
instance's env, never in the config repo.

- **GitHub App (recommended for webhook / server / community):** native per-install event delivery (the
  `installation` in the payload is the routing key, so edge-routing is "each install → the instance/channel that
  owns it"); its own least-privilege identity (posts as `the-bot[bot]`, read PRs + write review comments); and
  per-install short-lived tokens, so nothing long-lived sits on a server. Installable by anyone — the community
  distribution path.
- **User token (`gh`/PAT) for local or where an App can't be installed:** simplest for laptop/personal, today's
  exact behavior, and the **fallback for a company org** whose admins won't approve a custom App.

One behavior shift to decide per channel: as a **token** the natural trigger is "PRs where I'm *requested as
reviewer*"; as an **App** it is "PRs in *repos where the App is installed*" (optionally filtered by label/paths),
which is arguably the better model for a reviewer bot.

**D4 — Surface: a pluggable notifier ("notify + link") first-class, with the reachable web UI as the base;
interactive handling a later extension.**

- **Base — the web UI**, which gains a configurable **bind address + auth token** so it can be served beyond
  localhost. That is the "handle from anywhere" path; the actual hosting stays out of scope.
- **Notifier (outbound)** — the bot pings a surface when a review needs attention, with a link to the page or PR:
  a Slack incoming-webhook / bot token, or a generic webhook others adapt to Discord / Teams / email. One-way,
  small, community-friendly. This is the first-class integration.
- **Interactive (approve / post / discard from Slack buttons)** — a documented *future* extension behind the same
  surface interface. It needs a Slack app and a verified callback endpoint (a big, tool-specific surface), so it
  is deliberately not v1; "notify + link" plus the reachable web UI already cover "check & handle from anywhere."

Per channel: `surface: { web: { bind, auth }, notify: { slack | webhook | … } }`.

**D5 — State: the PR is the durable record; on-demand runs stateless by default, with a pluggable store
(none / local file / external) for cross-run analytics only.**

- **Correctness needs no store.** Idempotency ("already reviewed this SHA?") and incremental re-review ("what did
  my last review cover?") are derived by reading the bot's own watermarked comments on the PR. This is what lets an
  on-demand run (a GitHub Action with no persistent volume) work, and it makes service mode less reliant on local
  state too.
- **The store is optional and pluggable,** needed only for the accumulated analytics — the Stats page, the trust
  floor, the verbatim/downstream trends: `none` (stateless on-demand / CI, the default there), `local file`
  (laptop service mode), `external / volume` (server or k8s, or an on-demand deployment that opts into metrics).
  One store interface; SQLite-on-a-volume covers local + k8s, a shared DB (Postgres) a later extension.
- **Honest limitation:** the Stats/metrics views are a service-mode feature. Pure on-demand reviews fine but
  won't build trend views unless pointed at a store.

**D6 — A git "data store" for durable analytics (refines D5), which also gives idempotency.**
The pluggable store gains a third backend beside `local file` and `external DB`: a **git data repo** the bot
auto-commits to, Backlog.md-style — durable, versioned, and shareable with no database server (a team reads the
repo; an individual clones it). The D5 split holds: operational / in-flight state (reviews running, stages, the
queue) stays in SQLite — it churns per review and is written concurrently, so git would be noise and push races.
Only the **durable outcome records** go to git: per review, the verbatim/edited label, the downstream signal, cost,
decision, and the reviewed SHA — slow, append-mostly, one commit per review.

Why it earns its place:
- **It closes the ephemeral-analytics gap.** A stateless CI run reviews the PR, then commits its outcome record to
  the data repo, so analytics accumulate in git even though the compute kept nothing — "run anywhere" and "keep the
  dashboard" stop being a trade-off.
- **Idempotency for free.** A durable "reviewed `owner/repo#N` at `sha`" record answers "did I already do this?"
  without deriving it from the PR — so it **supersedes the hard part of P5.2** (reading the last-reviewed SHA back
  off GitHub, which is fragile because the bot posts as the user's own account and the watermark is not always in a
  review's body).

Guardrails (so it stays safe):
- **Labels and numbers only, never sensitive text or code.** The classifications are labels; cost and decision are
  numbers. Raw finding text and code snippets stay on GitHub (the posted comments); the data repo is **private**.
- **Serialize the commits** (a commit queue, or rebase-and-push like Backlog.md's post-commit hook) so two reviews
  finishing together do not collide on push. One commit per review, or batched snapshots if that is too noisy.

So D5's store options read: **none / local file / git data repo / external DB**, with operational state always in
SQLite.

## Rough shape of the work (not yet planned in detail)

- Load config from a git-checked-out file set; introduce the **channel** model + routing
  (whitelist / blacklist patterns).
- Put the **trigger** behind an interface: poll (service) and event (webhook / Action).
- Make **Claude auth** (config dir / OAuth token / API key) and **GitHub identity** (App /
  token) per-channel, injected from env — config references them, secrets stay out of git.
- Make **state** a pluggable store (none / local file / external), and derive idempotency and
  re-review from the PR.
- Add a **surface**: the web UI (configurable bind + auth) plus an outbound notifier.
- Package as a **container image**; keep the binary.
- *(Later, per deployment — out of this scope)* the Actions workflow, Helm chart, and server
  recipe.
