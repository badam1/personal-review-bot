# personal-review-bot

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
4. **Posts only what's confirmed, ≥ 80% certain and high severity** as one short COMMENT review. Every comment ends
   with the watermark and the certainty, e.g. `🤖 personal-review-bot · 92%`.
5. **Asks you about the rest.** Everything else, and every approval, waits under "Needs you" on a local page (and in
   the TUI), with a desktop notification. You pick what to post (and can edit it), approve, skip, or ignore the PR.

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
./install.sh          # builds ~/.local/bin/personal-review-bot and (re)starts the background service
./install.sh --build  # build only
```

On macOS the service is a launchd agent (log: `~/Library/Logs/personal-review-bot.log`); on Linux, a systemd user
service (log: `journalctl --user -u personal-review-bot`; run `loginctl enable-linger` to keep it running while you're
logged out). It gets the `PATH` you ran `./install.sh` from, so it finds `gh`, `git` and `claude` the way your shell
does: re-run the script after installing them somewhere new.

On first start it records the review requests that are already open as skipped instead of reviewing them all at once;
press `r` on one in the TUI to review it.

## Use

```sh
personal-review-bot                       # terminal UI: what's running and the last 10 reviews
personal-review-bot status                # same, printed once
personal-review-bot review --dry-run URL  # try it on any PR: posts nothing, opens nothing
personal-review-bot queue URL             # hand a PR to the running service
personal-review-bot agents                # the reviewers and prompts reviews run with, and where each comes from
```

The page is <http://localhost:12346>:

- **Reviews** (`/`): what needs you (the tab title counts it), what is running, failed reviews to retry, the last 10.
  Refreshes every minute.
- **A review** (`/r/N`): each finding with the code around its line, why the PR got its tier, and what to post or
  approve. On a re-review, "Earlier findings · 3 fixed · 1 still open" with the follow-up's evidence (approving while
  a high one is still open asks once more). A posted finding that turned out wrong can be marked wrong: the stats
  count it and re-reviews of the PR are told.
- **A PR's timeline** (`/pr/owner/repo/N`): every review of it, what started each, and what became of each finding.
- **History** (`/history`): every review, filtered by repo, author or status; ignored PRs, with "Un-ignore".
- **Stats** (`/stats`): spend, findings and their fate per reviewer, model, PR size and repo, spend per day, and the
  plan's usage.

## How it decides who asked

It polls GitHub every 2 minutes for open PRs with `user-review-requested:<you>`: a direct request, not a team
request. Set `ORG` to watch one org only.

## Settings

Each setting is read from a `PERSONAL_REVIEW_BOT_<NAME>` environment variable, then from
`~/.config/personal-review-bot/config.env` (`$XDG_CONFIG_HOME` is respected; `PERSONAL_REVIEW_BOT_CONFIG_DIR` moves
it), then falls back to its default. The background service reads the file, so that's where to put them:

```sh
# ~/.config/personal-review-bot/config.env
ORG=my-company
WATERMARK=🤖 my-review-bot
AUTO_SEVERITY=medium
```

Restart the service after changing it (`./install.sh`).

| Name | Default |
|---|---|
| `ORG` | empty: review requests from any org. Set it to only review PRs in that GitHub org; reviewers also search it for related repos (otherwise the PR repo's owner) |
| `THRESHOLD` | `80`: auto-post a confirmed finding at or above this certainty |
| `AUTO_SEVERITY` | `high`: lowest severity posted without asking (`high`, `medium`, `low`; anything else means `high`). The stats page's "What became of findings, by score" table shows whether the rule fits |
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

`prompts/review.md` (used by every reviewer), `verify.md`, `design.md` and `followup.md` replace the built-in ones.
They are Go [`text/template`](https://pkg.go.dev/text/template)s with these fields: `.Repo`, `.Number`, `.Title`,
`.Author`, `.Body`, `.Base`, `.Head`, `.Diff`, `.HeadSHA`, `.PrevSHA` (empty on a first review), `.Conversation`,
`.Prior` (earlier findings and their fate), `.Owner` (your login), `.Org`, `.Context`, plus `.Area` and `.Focus` in
the review prompt and `.Findings` (JSON) in the verify and follow-up prompts. Keep each asking for the same answer as
the built-in one: the JSON shape is enforced, and a run that doesn't match it fails.

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
- **Reviews leave no transcripts.** They run with `--no-session-persistence`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
