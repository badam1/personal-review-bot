# <img src="internal/web/icon.svg" alt="" width="40" align="top"> personal-review-bot

[![CI](https://github.com/badam1/personal-review-bot/actions/workflows/ci.yml/badge.svg)](https://github.com/badam1/personal-review-bot/actions/workflows/ci.yml)

A PR review bot that works for **you**, from your laptop. When someone requests your review on GitHub (you directly,
not a team you're in), it reviews the PR with [Claude Code](https://claude.com/claude-code) agents, has every finding
checked by a separate verifier, posts only what is confirmed and serious, and asks you about everything else on a
local page. **It never approves on its own.**

No webhook, server or org-admin rights: it polls GitHub with your `gh` login and runs `claude -p` with your Claude
login.

## How it works

1. **Sizes the PR.** Lines changed and risky paths (Terraform/Terragrunt, IAM/auth, prod, migrations…) pick the tier:
   small → Haiku reviewers, medium → Sonnet, large or risky → Opus. The verifier is never weaker than Sonnet.
   It also checks how full your Claude plan's limits are (the reading every `claude -p` run reports, or a ~$0.001
   Haiku call when the last one is over 10 minutes old, since your own use fills the same limits). From 75% of the
   5-hour or weekly limit every model is a step cheaper (Fable → Opus → Sonnet → Haiku) at medium effort; from 90% the
   reviewers run on Haiku at low effort and the design review is left out. The TUI shows the reading.
   A **deep review** you ask for (below) runs any PR as if it were large, without these savings.
2. **Reviews in parallel.** Logic, security, integration with other services, plus infra/rollout when infra files
   change; or your own set of reviewers (see [Customize](#customize-the-reviewers-and-prompts)). The reviewers run `claude -p` read-only inside a checkout of the PR head; they can read the other repos of
   the org (with `gh search code` and `gh api`) but can't post anything.
   On the first review of a medium or large PR, any PR touching an ADR, and every review you start yourself
   (Re-review, `r` in the TUI, `review`/`queue`), a **design reviewer** also judges how the PR solves its problem
   rather than its lines: the goal, fit with the repo's `AGENTS.md`/`CLAUDE.md`, its ADRs, `.claude/skills` practices
   and existing precedent, and simpler alternatives. It runs on Opus, or on Fable for large PRs (at high effort,
   stopped at $5 a run), and returns a short assessment and at most three concerns: the problem, the better way and
   the evidence, with its own confidence. Concerns skip the verifier and are **never posted without you**.
   On a re-review, a **follow-up check** (Sonnet, ~$0.15) asks of every finding posted on the PR before whether it was
   fixed, partly fixed, is still open, no longer applies, or the author declined it, with one line of evidence.
3. **Verifies.** A separate verifier re-checks every finding against the code, without seeing the reviewer's own
   confidence, and rates two things apart: how certain it is the problem is real, and how severe it is if merged.
   It also says how confident it is that nothing else stands in the way of approving, from its own check and what
   each reviewer says it examined; it runs for that when nothing was found too. A Haiku summary (about a cent) says
   what the PR changes and why, at the top of its page.
4. **Posts only what's confirmed, ≥ 80% certain and of low or medium severity** as one short COMMENT review. Every
   comment ends with the watermark and the certainty, e.g. `🤖 personal-review-bot · 92%`.
5. **Asks you about the rest.** High-severity findings (the costly kind to get wrong under your name), design
   concerns, everything less certain, and every approval wait under "Needs you" on a local page (and in the TUI),
   with a desktop notification. You pick what to post (and can edit it), approve, skip, or ignore the PR.

## Requirements

- macOS or Linux.
- [Go](https://go.dev/dl/) (the version in `go.mod`) to build it.
- [`gh`](https://cli.github.com/), logged in (`gh auth login`). Repos are cloned over the protocol `gh` is set to
  (`gh config get git_protocol`), with `gh`'s own credentials, so nothing else needs setting up.
- [Claude Code](https://claude.com/claude-code) (`claude`), logged in with a Claude plan or an API key. Reviews run
  on your account and count towards its limits or bill.
- `git`.
- Optional: [`terminal-notifier`](https://github.com/julienXX/terminal-notifier) on macOS (clicking the notification
  opens the review), `notify-send` on Linux.

## Install

```sh
git clone https://github.com/badam1/personal-review-bot.git
cd personal-review-bot
git checkout "$(git describe --tags --abbrev=0)"  # the latest release; stay on main for unreleased changes
./install.sh          # builds ~/.local/bin/personal-review-bot and (re)starts the background service
./install.sh --build  # build only
```

To update, use the page's button, `u` in the TUI or `personal-review-bot update` (see [Updating](#updating)).
`personal-review-bot version` says which one runs. Until 1.0, a minor release (0.x.0) may change settings or file
formats, and its notes say what to change.

[Releases](https://github.com/badam1/personal-review-bot/releases) also have prebuilt binaries for macOS and Linux
(arm64 and amd64), with checksums and build attestations, to run `personal-review-bot` without Go. Check where one
was built with `gh attestation verify <file> -R badam1/personal-review-bot`. They aren't notarized by Apple: on
macOS, run `xattr -d com.apple.quarantine personal-review-bot` once on a downloaded one. `install.sh` builds from
source.

On macOS the service is a launchd agent (log: `~/Library/Logs/personal-review-bot.log`); on Linux, a systemd user
service (log: `journalctl --user -u personal-review-bot`; run `loginctl enable-linger` to keep it running while you're
logged out). It gets the `PATH` you ran `./install.sh` from, so it finds `gh`, `git` and `claude` the way your shell
does: re-run the script after installing them somewhere new.

On first start it records the review requests that are already open as skipped instead of reviewing them all at once;
press `r` on one in the TUI to review it.

## Use

```sh
personal-review-bot                       # terminal UI: what's running and the last 10 reviews, with their cost
personal-review-bot status                # same, printed once
personal-review-bot review --dry-run URL  # try it on any PR: posts nothing, opens nothing
personal-review-bot queue URL             # hand a PR to the running service
personal-review-bot queue --deep URL      # … as a deep review: Opus, the design review, whatever its size
personal-review-bot agents                # the reviewers and prompts reviews run with, and where each comes from
personal-review-bot version               # which build this is
personal-review-bot update [--check]      # install the newest release (--check: only say if there is one)
```

The page is <http://localhost:12346>. The bar on every page shows what is running (the working line, linked to it),
whether the daemon is watching GitHub, the plan's usage, and the version menu (updates, release notes, report a bug).

- **Reviews** (`/`): what needs you (the tab title counts it), what is running and the step it is on, failed reviews
  to retry, the last 10, and the spend today and over 7 days. Refreshes every minute.
- **A review** (`/r/N`): each finding with the code around its line, why the PR got its tier, and what to post or
  approve, with your decision beside them. On a re-review, the earlier findings counted ("3 fixed", "1 still open")
  with the follow-up's evidence (approving while a high one is still open asks once more). A posted finding that
  turned out wrong can be marked wrong: the stats count it and re-reviews of the PR are told.
- **Deep review**: on any review, the **Deep review** button (or `d` in the TUI, `--deep` for `review` and `queue`)
  reviews the PR again at its latest commit as if it were large: Opus reviewers and verifier, the design review and
  the follow-up check on Opus, at each model's default effort, never the cheaper models or lower effort the usage
  savings switch to. Use it for a small or medium PR that deserves more scrutiny. It shows as tier "deep" (on the
  stats page too, so its cost stays apart from the sizes), and a retry of a failed deep review stays deep; automatic
  re-reviews after it are sized as usual.
- **Reminders**: on a re-review, every earlier finding that is still open or only partly fixed can be reminded of,
  with a text you can edit ("Still open at `abc1234`: …"). The ones nobody answered start ticked; where the author
  replied, the reply is shown and you decide. Ticked reminders go out with the review you post, as one notification:
  a reply in the finding's thread (reopening it if it was resolved), or, for design concerns and findings outside
  the diff, a list in the review body. A reminded finding isn't ticked again until the author pushes.
- **A PR's timeline** (`/pr/owner/repo/N`): every review of it, what started each, and what became of each finding.
- **History** (`/history`): every review, filtered by repo, author or status; ignored PRs, with "Un-ignore".
- **Stats** (`/stats`): spend, findings and their fate per reviewer, model, PR size and repo, spend per day, and the
  plan's usage.

## Updating

The daemon looks for a new release when it starts and every hour; **Check for updates** in the version menu (the
version in the bar) looks right away. When there is one, the version menu and the TUI say so, with the first lines of
its notes; nothing is installed until you ask:

- the **Update and restart** button in the version menu, which then shows each step until it restarts; `u` in the
  TUI; or `personal-review-bot update` (the last two look for the newest release themselves);
- it downloads the release's binary for your OS and CPU, checks it against the release's checksums and its GitHub
  build attestation (built by this repo's release workflow, checked with `gh attestation verify`), and puts it in
  place of the installed binary;
- the daemon then takes no new reviews, lets the running ones finish, and restarts into the new version in place
  (same process, so launchd, systemd or a terminal keep it);
- the page follows each step live (downloading, checking, waiting for N running reviews, restarting) and then says
  "✓ Updated to v…" for an hour; the TUI and `personal-review-bot update` show the same steps. A TUI that was open
  during the update still runs the old version and says to restart it.

While you run a beta you are offered betas; a stable release only offers stable ones. Without a running daemon,
`personal-review-bot update` just replaces the binary. Only the binary is updated: when a release changes the
service setup, its notes say to run `./install.sh` from it.

## How it decides who asked

It polls GitHub every 2 minutes for open PRs with `user-review-requested:<you>`: a direct request, not a team
request. Set `ORG` to watch one org, or several, only.

## Settings

Each setting is read from a `PERSONAL_REVIEW_BOT_<NAME>` environment variable, then from
`~/.config/personal-review-bot/config.env` (`$XDG_CONFIG_HOME` is respected; `PERSONAL_REVIEW_BOT_CONFIG_DIR` moves
it), then falls back to its default. The background service reads the file, so that's where to put them:

```sh
# ~/.config/personal-review-bot/config.env
ORG=my-company,my-other-org
WATERMARK=🤖 my-review-bot
AUTO_POST=low
```

Restart the service after changing it (`./install.sh`).

| Name | Default |
|---|---|
| `ORG` | empty: review requests from any org. One org, or several separated by commas or spaces (`acme,umbrella`), to only review PRs in those; a value that isn't an organisation name is refused at startup rather than searched for. Reviewers search one org for related repos: the PR's own when it is one you set, the single org you set for a PR queued from outside it, otherwise the PR repo's owner |
| `THRESHOLD` | `80`: auto-post a confirmed finding at or above this certainty |
| `AUTO_POST` | `low,medium`: the severities posted without asking, once confirmed at `THRESHOLD`. High ones wait for you; `high` alone posts only those, `low,medium,high` everything, empty nothing. A misspelt word is left out. The stats page's "What became of findings, by score" table shows whether the rule fits. (The older `AUTO_SEVERITY`, "this severity or higher", still works when `AUTO_POST` isn't set.) |
| `SHOW_LABELS` | `risk/,auto-risk/`: the PR labels shown in the lists' Risk column and beside the size on a review, by prefix (comma-separated; empty shows none). Coloured by the level they end in: `/high`, `/medium`, `/low` |
| `APPROVE_TEXT` | `LGTM :+1:`: said with your first approval of a PR; a re-approval (after new commits dismissed it) goes without it; empty = approve without a comment |
| `WATERMARK` | `🤖 personal-review-bot`: ends every comment the bot posts, followed by the certainty |
| `SAVE_AT` | `75`: % of the Claude plan's 5-hour or weekly limit from which reviews use cheaper models at medium effort |
| `MIN_AT` | `90`: % from which they use the cheapest (Haiku reviewers, Sonnet verifier) at low effort, without the design review |
| `POLL_SECONDS` | `120` |
| `PARALLEL` | `2` reviews at once |
| `AGENT_TIMEOUT_MINUTES` | `20` per Claude run |
| `SKIP_DRAFTS` | `1` |
| `MAX_AGE_DAYS` | `14`: PRs with no activity (commits, comments, reviews, edits) for longer are skipped automatically; a skipped PR is reviewed once it sees activity again, and `r` in the TUI reviews it anyway; `0` = no limit |
| `ADDR` | `127.0.0.1:12346`: the local page |
| `UPDATE_CHECK` | `1`: look for new releases every hour; `0` = only when you ask (Check for updates, `u`, `personal-review-bot update`) |
| `UPDATE_REPO` | `badam1/personal-review-bot`: where releases come from, for a fork that publishes its own (its release workflow must attest the binaries the same way) |
| `STATE_DIR` | `~/.local/state/personal-review-bot` (`$XDG_STATE_HOME`): the SQLite database |
| `CACHE_DIR` | `~/.cache/personal-review-bot` (`$XDG_CACHE_HOME`): repo clones and review checkouts |

## Customize the reviewers and prompts

Everything the agents are told is in Markdown files you can change, in the config directory:

```text
~/.config/personal-review-bot/
├── config.env
├── context.md          notes every prompt gets: where your ADRs live, how tasks are tracked, house rules
├── agents/
│   ├── security.md     changes the built-in security reviewer
│   ├── integration.md  "enabled: false" switches it off
│   └── cost.md         a reviewer of your own
└── prompts/
    └── design.md       replaces the built-in design prompt
```

Start from the built-in files: `personal-review-bot agents export /tmp/prb` writes them out, with an example
`context.md`. Copy only the files you change: a copied prompt no longer gets improvements from new versions.
`personal-review-bot agents` shows what reviews will run with and checks your files. Changes apply from the next
review, without a restart. A file that can't be used fails the review with its name and the problem, rather than
reviewing without it.

### Reviewers

A reviewer is `agents/<name>.md`: a front matter block, then what to look for.

```markdown
---
area: cloud cost         # what it reviews, in a few words (required for a new reviewer)
paths: \.(tf|tfvars)$    # Go regexp: runs only when a changed file matches; leave it out to run on every PR
docs: false              # true: also runs on PRs that only change docs
model: opus              # pin a model (alias or full ID); leave it out for the PR tier's
enabled: true            # false switches the reviewer off
---
Resources sized or priced above what the change needs: instance types, replicas, storage classes, missing
lifecycle rules. Report only what costs real money, with the cheaper setting.
```

- The file name is the reviewer's name: lowercase letters, digits and dashes (`design`, `followup` and `verifier`
  are taken).
- A file named like a built-in reviewer (`logic`, `security`, `integration`, `infra`) changes only the fields it
  sets, and the focus if its body isn't empty. `model: opus` alone pins the security reviewer to Opus.
- Front matter is `key: value` lines; ` #` starts a comment unless the value is quoted.
- Every reviewer runs with the shared review prompt, its area and focus filled in, and its findings go to the
  verifier like the others. A pinned model gets cheaper with the usage limits like every other model.

### Prompts

`prompts/review.md` (used by every reviewer), `verify.md`, `design.md`, `followup.md` and `summary.md` (the two
sentences at the top of a review's page) replace the built-in ones.
They are Go [`text/template`](https://pkg.go.dev/text/template)s with these fields: `.Repo`, `.Number`, `.Title`,
`.Author`, `.Body`, `.Base`, `.Head`, `.Diff`, `.HeadSHA`, `.PrevSHA` (empty on a first review), `.Conversation`,
`.Prior` (earlier findings and their fate), `.Owner` (your login), `.Org`, `.Context`, plus `.Area` and `.Focus` in
the review prompt, `.Findings` (JSON) in the verify and follow-up prompts, and `.Accounts` (what each reviewer says
it examined) in the verify prompt. Keep each asking for the same answer as the built-in one: the JSON shape is
enforced, and a run that doesn't match it fails.

### Context

`context.md` is added to all four prompts before the PR, marked as written by you, so the agents can rely on it
(unlike the PR's description and conversation). Use it for what they can't find in the repo: where org-wide ADRs
live, how to find the task behind a PR, which shared workflows or modules everything should use.

## Safety

- **No GitHub writes from the reviewers.** They run with read-only tools only (Read, Grep, Glob and read-only
  `git`/`gh` commands). Only the bot's own code posts, and only COMMENT reviews (plus the approvals you click). Each
  Claude run has `PERSONAL_REVIEW_BOT_AGENT=1` set, so your own Claude Code hooks can recognise them and add rules.
- **Unverified findings never auto-post.** If the verifier fails, every finding goes to the page.
- **The page can't be triggered from outside.** It listens on 127.0.0.1 only, checks the Host header (against DNS
  rebinding), rejects cross-site requests, requires a secret token on every action, and runs only its own scripts
  (no inline JavaScript).
- **Updates are verified.** A new binary is installed only if it matches the release's checksum and GitHub attests it
  was built by the release workflow of `UPDATE_REPO`; you start every update.
- **Reviews leave no transcripts.** They run with `--no-session-persistence`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
