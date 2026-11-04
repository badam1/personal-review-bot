# Contributing

Thanks for helping. Issues and pull requests are welcome; for anything bigger than a fix, open an issue first so we
can agree on the approach before you spend time on it.

## Set up

You need Go (the version in `go.mod`), `gh` and `git`. Claude Code (`claude`) is only needed for dry runs and live
tests.

```sh
git clone https://github.com/badam1/personal-review-bot.git
cd personal-review-bot
go test ./...
./install.sh --build   # builds ~/.local/bin/personal-review-bot without touching a running service
```

## Making a change

1. Branch from `main`: `feat/<short-name>`, `fix/<short-name>`, `docs/…`, `chore/…`.
2. Keep the PR to one change. Match the code around it: naming, comment style, and tests next to the code they
   test.
3. Before pushing:

   ```sh
   gofmt -l .        # must print nothing
   go vet ./...
   go test ./...
   ```

4. Open a PR against `main` and fill in the template: Description, Testing, Risk, and Out of scope when it helps.
5. PRs are squash-merged, so the PR title becomes the commit on `main`. It must follow
   [Conventional Commits](https://www.conventionalcommits.org/): `feat: …`, `fix: …`, `docs: …`, `refactor: …`,
   `test: …`, `ci: …`, `chore: …`, `build: …`, `perf: …` or `revert: …`, with an optional scope (`fix(web): …`)
   and `!` for a breaking change (`feat!: …`). A check enforces it.

CI runs gofmt, `go vet`, `go test` and shellcheck on every PR; it has to pass before merging.

## Testing against real PRs

Unit tests never call GitHub or Claude. To see a change on a real PR without posting anything:

```sh
go build -o /tmp/prb . && /tmp/prb review --dry-run https://github.com/<owner>/<repo>/pull/<n>
```

The live tests run real `gh` and `claude` calls and are skipped unless their variable is set, for example:

```sh
PERSONAL_REVIEW_BOT_LIVE_PR=https://github.com/<owner>/<repo>/pull/<n> go test ./internal/gh -run Live -v
```

See the `*_live_test.go` files for the others (`PERSONAL_REVIEW_BOT_LIVE_DESIGN`, `…_LIVE_FOLLOWUP`,
`…_LIVE_CLAUDE`, `…_LIVE_MODEL`).

**Prompt and reviewer changes** (`internal/agents/prompts/`, `internal/agents/reviewers/`) change what gets posted
on other people's PRs. Run a dry run or a live test on at least one real PR and say in the PR what changed in the
output.

## Ground rules

These keep the bot safe to run on other people's PRs; a change that weakens one needs a strong reason in its PR.

- Reviewers stay read-only: no write tools, no `gh` commands that change anything.
- Nothing is approved without the user clicking Approve.
- Only findings the verifier confirmed, above the configured certainty and severity, are posted without asking.
- The local page listens on 127.0.0.1 only, and every action needs its token.
- No secrets in logs, the database or the page.

## Reporting a security problem

Please don't open a public issue. Use GitHub's
[private vulnerability reporting](https://github.com/badam1/personal-review-bot/security/advisories/new) instead.
