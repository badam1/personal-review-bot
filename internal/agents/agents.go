// Package agents runs the Claude reviewers and the verifier as headless `claude -p` processes inside the PR
// checkout. They are read-only: their only tools are Read/Grep/Glob, a few read-only git commands, `gh search code`
// and gh-read, a GET of the GitHub API (see ghRead), and their
// output is schema-validated JSON. Each run has PERSONAL_REVIEW_BOT_AGENT=1 set, so your own Claude Code hooks can
// tell these runs apart (for example to block GitHub writes from them as well).
package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Finding struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	Comment    string `json:"comment"`
	Confidence int    `json:"confidence"`
}

// Candidate is a reviewer finding handed to the verifier. It leaves out the reviewer's own confidence, so the
// verifier scores the finding from the code rather than adjusting the reviewer's number.
type Candidate struct {
	ID       int64  `json:"id"`
	Agent    string `json:"agent"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Comment  string `json:"comment"`
}

func NewCandidate(id int64, agent string, f Finding) Candidate {
	return Candidate{ID: id, Agent: agent, Path: f.Path, Line: f.Line, Severity: f.Severity, Title: f.Title, Comment: f.Comment}
}

// Verdict scores two things separately: how sure the verifier is that the problem is real (certainty), and how much
// it matters if merged as is (severity).
type Verdict struct {
	ID        int64  `json:"id"`
	Verdict   string `json:"verdict"` // confirmed | plausible | rejected
	Certainty int    `json:"certainty"`
	Severity  string `json:"severity"` // high | medium | low
	Comment   string `json:"comment"`
	Line      int    `json:"line"`
}

type Input struct {
	Repo, Title, Author, Body, Base, Head, Diff string
	Number                                      int
	Owner                                       string // the GitHub login the bot reviews for
	Org                                         string // where to look for related repos: the configured org, else the repo's owner
	Dir                                         string // PR checkout
	HeadSHA                                     string // the commit being reviewed
	PrevSHA                                     string // the commit the last complete review covered ("" = first review)
	Conversation                                string // comments, reviews and inline threads so far
	Prior                                       string // earlier personal-review-bot findings on this PR and their fate
	Effort                                      string // how hard every run works ("" = the model's default)
}

// Run is what one Claude run did: the evidence behind its result, which matters most when it found nothing.
type Run struct {
	Turns    int
	Denials  int // tool calls that were refused: a reviewer that couldn't read the code also finds nothing
	Duration time.Duration
	Cost     float64
	Checked  string // the reviewer's own account of what it examined
	Usage    *Usage // the plan's rate-limit usage as of this run (nil if it reported none)
}

// The design reviewer judges how the PR solves its problem rather than its lines; triage picks its model.
const DesignAgent = "design"

// VerifierAgent checks every reviewer finding against the code before anything is posted.
const VerifierAgent = "verifier"

// SummaryAgent writes the two sentences at the top of a review's page: what the PR changes and why.
const SummaryAgent = "summary"

// SummaryModel is cheap and quick: the summary reads only its prompt.
const SummaryModel = "haiku"

// Fable costs 2.5× Opus per token, so unless the plan says otherwise it runs at high effort rather than the xhigh
// default (Fable at high often matches or beats Opus at xhigh), and a run that passes its cap stops instead of going on.
var (
	defaultEffort = map[string]string{"fable": "high"}
	modelArgs     = map[string][]string{"fable": {"--max-budget-usd", "5"}}
)

// Concern is one objection to a PR's design. Concerns are judgement, so they skip the verifier and are never
// posted without you; each must name the evidence it rests on and how sure the reviewer is that it holds.
type Concern struct {
	Title       string `json:"title"`
	Concern     string `json:"concern"`
	Evidence    string `json:"evidence"`
	Alternative string `json:"alternative"`
	Severity    string `json:"severity"`
	Confidence  int    `json:"confidence"`
}

const findingsSchema = `{"type":"object","required":["checked","findings"],"properties":{"checked":{"type":"string"},
  "findings":{"type":"array","items":{
  "type":"object","required":["path","line","severity","title","comment","confidence"],"properties":{
  "path":{"type":"string"},"line":{"type":"integer","minimum":1},"severity":{"enum":["high","medium","low"]},
  "title":{"type":"string"},"comment":{"type":"string"},"confidence":{"type":"integer","minimum":0,"maximum":100}}}}}}`

const verdictsSchema = `{"type":"object","required":["summary","verdicts","approval"],"properties":{"summary":{"type":"string"},
  "approval":{"type":"object","required":["confidence","reason"],"properties":{
  "confidence":{"type":"integer","minimum":0,"maximum":100},"reason":{"type":"string","minLength":20}}},
  "verdicts":{"type":"array","items":{"type":"object","required":["id","verdict","certainty","severity","comment","line"],
  "properties":{"id":{"type":"integer"},"verdict":{"enum":["confirmed","plausible","rejected"]},
  "certainty":{"type":"integer","minimum":0,"maximum":100},"severity":{"enum":["high","medium","low"]},
  "comment":{"type":"string"},"line":{"type":"integer","minimum":1}}}}}}`

const designSchema = `{"type":"object","required":["assessment","concerns"],"properties":{"assessment":{"type":"string"},
  "concerns":{"type":"array","maxItems":3,"items":{"type":"object",
  "required":["title","concern","evidence","alternative","severity","confidence"],"properties":{
  "title":{"type":"string"},"concern":{"type":"string"},"evidence":{"type":"string"},"alternative":{"type":"string"},
  "severity":{"enum":["high","medium","low"]},"confidence":{"type":"integer","minimum":0,"maximum":100}}}}}}`

const summarySchema = `{"type":"object","required":["about"],"properties":{"about":{"type":"string","minLength":80,"maxLength":600}}}`

const followupSchema = `{"type":"object","required":["followups"],"properties":{"followups":{"type":"array","items":{
  "type":"object","required":["id","status","note"],"properties":{"id":{"type":"integer"},
  "status":{"enum":["fixed","partly","open","moot","declined"]},"note":{"type":"string"}}}}}}`

// FollowupAgent checks whether findings posted on earlier reviews of the PR were addressed.
const FollowupAgent = "followup"

// Earlier is a finding posted on an earlier review, handed to the follow-up check with the commit it was made on.
type Earlier struct {
	ID      int64  `json:"id"`
	Commit  string `json:"commit"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Title   string `json:"title"`
	Comment string `json:"comment"`
}

// Followup is what became of one earlier finding.
type Followup struct {
	ID     int64  `json:"id"`
	Status string `json:"status"` // fixed | partly | open | moot | declined
	Note   string `json:"note"`
}

// MaxDiff caps the diff placed in the prompt; reviewers read files from the checkout for the rest.
const MaxDiff = 150_000

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated — read the files in the checkout for the rest)"
}

type promptData struct {
	Input
	Area, Focus, Findings string
	Accounts              string // what each reviewer says it examined, for the verifier's approval confidence
	Context               string // context.md: the owner's notes about their organisation
}

// prompt renders one of the kit's prompts.
func (k *Kit) prompt(name string, data promptData) (string, error) {
	data.Context = k.Context
	var b bytes.Buffer
	if err := k.prompts[name].Execute(&b, data); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	return b.String(), nil
}

// Review runs one reviewer and returns its findings and what the run did.
func (k *Kit) Review(ctx context.Context, r Reviewer, model string, in Input, timeout time.Duration) ([]Finding, Run, error) {
	in.Diff, in.Body = clip(in.Diff, MaxDiff), clip(in.Body, 4000)
	prompt, err := k.prompt("review", promptData{Input: in, Area: r.Area, Focus: r.Focus})
	if err != nil {
		return nil, Run{}, err
	}
	raw, ran, err := run(ctx, in.Dir, model, in.Effort, prompt, findingsSchema, timeout)
	if err != nil {
		return nil, ran, err
	}
	var out struct {
		Checked  string
		Findings []Finding
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ran, fmt.Errorf("parse %s findings: %w", r.Name, err)
	}
	ran.Checked = strings.TrimSpace(out.Checked)
	return out.Findings, ran, nil
}

// Design reviews how the PR solves its problem and returns its concerns; the run's Checked carries the overall assessment.
func (k *Kit) Design(ctx context.Context, model string, in Input, timeout time.Duration) ([]Concern, Run, error) {
	in.Diff, in.Body = clip(in.Diff, MaxDiff), clip(in.Body, 4000)
	prompt, err := k.prompt("design", promptData{Input: in})
	if err != nil {
		return nil, Run{}, err
	}
	raw, r, err := run(ctx, in.Dir, model, in.Effort, prompt, designSchema, timeout)
	if err != nil {
		return nil, r, err
	}
	var out struct {
		Assessment string
		Concerns   []Concern
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, r, fmt.Errorf("parse design concerns: %w", err)
	}
	r.Checked = strings.TrimSpace(out.Assessment)
	if len(out.Concerns) > 3 {
		out.Concerns = out.Concerns[:3]
	}
	return out.Concerns, r, nil
}

// CheckEarlier asks whether the earlier findings were fixed, from the code at the PR's head and the replies to them.
func (k *Kit) CheckEarlier(ctx context.Context, model string, in Input, earlier []Earlier, timeout time.Duration) ([]Followup, Run, error) {
	js, _ := json.MarshalIndent(earlier, "", "  ")
	in.Body = clip(in.Body, 4000)
	prompt, err := k.prompt("followup", promptData{Input: in, Findings: string(js)})
	if err != nil {
		return nil, Run{}, err
	}
	raw, r, err := run(ctx, in.Dir, model, in.Effort, prompt, followupSchema, timeout)
	if err != nil {
		return nil, r, err
	}
	var out struct{ Followups []Followup }
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, r, fmt.Errorf("parse follow-ups: %w", err)
	}
	return out.Followups, r, nil
}

// Summarize says what the PR changes and why, for the top of its page. It has no tools: it reads only the prompt,
// whose description and diff come from people the bot doesn't control.
func (k *Kit) Summarize(ctx context.Context, in Input, timeout time.Duration) (string, Run, error) {
	in.Diff, in.Body = clip(in.Diff, 60_000), clip(in.Body, 4000)
	prompt, err := k.prompt("summary", promptData{Input: in})
	if err != nil {
		return "", Run{}, err
	}
	raw, r, err := runWith(ctx, in.Dir, SummaryModel, "", prompt, summarySchema, timeout, noTools)
	if err != nil {
		return "", r, err
	}
	var out struct{ About string }
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", r, fmt.Errorf("parse summary: %w", err)
	}
	return strings.TrimSpace(out.About), r, nil
}

// Text is the concern as it would be posted: the problem, the better way, then the references.
func (c Concern) Text() string {
	parts := []string{strings.TrimSpace(c.Concern)}
	for _, p := range [][2]string{{"Instead", c.Alternative}, {"Evidence", c.Evidence}} {
		if v := strings.TrimSpace(p[1]); v != "" {
			parts = append(parts, fmt.Sprintf("**%s:** %s", p[0], v))
		}
	}
	return strings.Join(parts, "\n\n")
}

// Verification is the verifier's answer: a verdict per finding, the PR's state in a sentence, and how confident it is
// that nothing beyond its findings stands in the way of approving.
type Verification struct {
	Verdicts []Verdict
	Summary  string
	Approval Approval
}

// Approval is how confident the verifier is that the PR can be approved once its findings are dealt with: nothing
// the reviewers missed in what the diff changes. The reason names what it rests on and the biggest gap left.
type Approval struct {
	Confidence int    `json:"confidence"`
	Reason     string `json:"reason"`
}

// Verify re-checks every candidate against the code and returns a verdict and confidence per finding, and its
// confidence in approving. It runs with no candidates too: then the approval rests on its own check of the diff and
// on what the reviewers' accounts say they examined.
func (k *Kit) Verify(ctx context.Context, model string, in Input, cands []Candidate, accounts string, timeout time.Duration) (Verification, Run, error) {
	js, _ := json.MarshalIndent(cands, "", "  ")
	if len(cands) == 0 {
		js = []byte("[]")
	}
	in.Diff, in.Body = clip(in.Diff, MaxDiff), clip(in.Body, 4000)
	prompt, err := k.prompt("verify", promptData{Input: in, Findings: string(js), Accounts: accounts})
	if err != nil {
		return Verification{}, Run{}, err
	}
	raw, r, err := run(ctx, in.Dir, model, in.Effort, prompt, verdictsSchema, timeout)
	if err != nil {
		return Verification{}, r, err
	}
	var out Verification
	if err := json.Unmarshal(raw, &out); err != nil {
		return Verification{}, r, fmt.Errorf("parse verdicts: %w", err)
	}
	out.Summary, out.Approval.Reason = strings.TrimSpace(out.Summary), strings.TrimSpace(out.Approval.Reason)
	return out, r, nil
}

// Usage is how full the Claude plan's rate limits are, as `claude -p` reports with every run: the 5-hour session
// window and the weekly one, each 0-1, with when each resets.
type Usage struct {
	FiveHour       float64   `json:"five_hour"`
	FiveHourResets time.Time `json:"five_hour_resets"`
	Week           float64   `json:"week"`
	WeekResets     time.Time `json:"week_resets"`
	Seen           time.Time `json:"seen"` // when it was read
}

// At is how full the two windows are at now; a window past its reset counts as empty.
func (u Usage) At(now time.Time) (five, week float64) {
	five, week = u.FiveHour, u.Week
	if !u.FiveHourResets.IsZero() && now.After(u.FiveHourResets) {
		five = 0
	}
	if !u.WeekResets.IsZero() && now.After(u.WeekResets) {
		week = 0
	}
	return five, week
}

// Peak is the fuller window at now and its name ("5h" or "week").
func (u Usage) Peak(now time.Time) (float64, string) {
	five, week := u.At(now)
	if five > week {
		return five, "5h"
	}
	return week, "week"
}

// CheckUsage reads the plan's usage with the smallest call there is (Haiku, low effort, no tools: about $0.001).
func CheckUsage(ctx context.Context) (Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, u, err := claude(ctx, os.TempDir(), "Reply with: ok", "--model", "haiku", "--effort", "low", "--tools", "")
	switch {
	case err != nil:
		return Usage{}, err
	case u == nil:
		return Usage{}, fmt.Errorf("claude reported no usage (subtype %q)", res.Subtype)
	}
	return *u, nil
}

// result is the last line `claude -p --output-format stream-json` prints.
type result struct {
	IsError          bool              `json:"is_error"`
	Subtype          string            `json:"subtype"`
	Result           string            `json:"result"`
	StructuredOutput json.RawMessage   `json:"structured_output"`
	Cost             float64           `json:"total_cost_usd"`
	Turns            int               `json:"num_turns"`
	DurationMS       int64             `json:"duration_ms"`
	Denials          []json.RawMessage `json:"permission_denials"`
}

// baseArgs go with every run. The PR's checkout is the working directory and its author controls it, so only your own
// user settings apply: --setting-sources user leaves out the checkout's .claude/settings.json (hooks there would run
// shell commands on this machine) and its CLAUDE.md (instructions there would steer the reviewers), and
// --strict-mcp-config its .mcp.json.
var baseArgs = []string{"-p", "--output-format", "stream-json", "--verbose", "--setting-sources", "user",
	"--strict-mcp-config",
	"--no-session-persistence"} // no transcripts: they'd clutter ~/.claude and mix with your own sessions

// claude runs `claude -p` with the prompt on stdin (diffs can exceed the argv limit) and streams its events, which,
// unlike the plain JSON output, carry the plan's rate-limit usage.
func claude(ctx context.Context, dir, prompt string, args ...string) (*result, *Usage, error) {
	args = append(slices.Clone(baseArgs), args...)
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	bin, err := commandDir()
	if err != nil {
		return nil, nil, fmt.Errorf("gh-read: %w", err)
	}
	// The last PATH in the list is the one the run gets.
	cmd.Env = append(os.Environ(), "PERSONAL_REVIEW_BOT_AGENT=1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res, u := parseStream(stdout.Bytes(), time.Now())
	if res == nil {
		if ctx.Err() != nil {
			return nil, u, ctx.Err()
		}
		return nil, u, fmt.Errorf("%v: %s", runErr, clip(strings.TrimSpace(stderr.String()), 400))
	}
	return res, u, nil
}

// parseStream picks the result and the latest usage reading out of a stream-json run's events.
func parseStream(out []byte, now time.Time) (*result, *Usage) {
	var res *result
	var u *Usage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var ev struct {
			Type      string `json:"type"`
			RateLimit struct {
				Windows map[string]struct {
					Utilization float64 `json:"utilization"`
					ResetsAt    int64   `json:"resetsAt"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return res, u
		}
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "result":
			res = new(result)
			if json.Unmarshal(raw, res) != nil {
				res = nil
			}
		case "rate_limit_event":
			five, week := ev.RateLimit.Windows["five_hour"], ev.RateLimit.Windows["seven_day"]
			if five.ResetsAt == 0 && week.ResetsAt == 0 {
				continue // an API key, not a plan: no windows to fill
			}
			u = &Usage{FiveHour: five.Utilization, Week: week.Utilization, Seen: now,
				FiveHourResets: time.Unix(five.ResetsAt, 0), WeekResets: time.Unix(week.ResetsAt, 0)}
		}
	}
}

// readOnly is what reviewers and the verifier may use: the checkout, and a few commands that only read.
//
// GitHub is read through gh-read, never `gh api`: the same `gh api` writes with your token when given -X, --method
// or a field (-f, -F, --input), and a permission rule only sees the command's text, which quoting a flag gets past
// ('-fname=x' and "-X" POST both reached gh as writes past a deny list naming them). gh-read checks the arguments
// the shell actually hands it. `gh api` itself is denied outright. (Found by @krisztiankoszegi in #24.)
var readOnly = []string{"--permission-mode", "default",
	"--tools", "Read", "Grep", "Glob", "Bash",
	"--allowedTools", "Read", "Grep", "Glob",
	"Bash(gh search code *)", "Bash(gh-read *)", "Bash(git log *)", "Bash(git show *)", "Bash(git diff *)", "Bash(git blame *)",
	"--disallowedTools", "Bash(gh api *)"}

// ghRead is the gh-read command the runs find first on their PATH: one GET of the GitHub API. It takes exactly one
// argument, refuses one that is a flag or another host, and passes it after "--", where gh can only take it for the
// endpoint, so no form of the call can change the method or add a body.
const ghRead = `#!/bin/sh
# gh-read: a GET of the GitHub API and nothing else, for personal-review-bot's reviewers.
if [ "$#" -ne 1 ]; then
  echo "gh-read takes exactly one endpoint, e.g. gh-read repos/OWNER/REPO/contents/PATH" >&2
  exit 2
fi
case "$1" in
  -*|*://*) echo "gh-read: not an API endpoint: $1" >&2; exit 2 ;;
esac
exec gh api --method GET -- "$1"
`

var (
	binOnce sync.Once
	binDir  string
	binErr  error
)

// commandDir writes gh-read into a private directory of its own, once, and returns the directory, for the front of
// the runs' PATH.
func commandDir() (string, error) {
	binOnce.Do(func() {
		if binDir, binErr = os.MkdirTemp("", "personal-review-bot-bin-"); binErr == nil {
			binErr = os.WriteFile(filepath.Join(binDir, "gh-read"), []byte(ghRead), 0o755)
		}
	})
	return binDir, binErr
}

// noTools is for a run that needs nothing beyond its prompt.
var noTools = []string{"--tools", ""}

// run executes one read-only reviewer or verifier run and returns its structured output.
func run(ctx context.Context, dir, model, effort, prompt, schema string, timeout time.Duration) (json.RawMessage, Run, error) {
	return runWith(ctx, dir, model, effort, prompt, schema, timeout, readOnly)
}

// runWith executes one run with the given tools and returns its structured output.
func runWith(ctx context.Context, dir, model, effort, prompt, schema string, timeout time.Duration, tools []string) (json.RawMessage, Run, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string{"--model", model, "--json-schema", schema}, tools...)
	if effort == "" {
		effort = defaultEffort[model]
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	res, u, err := claude(ctx, dir, prompt, append(args, modelArgs[model]...)...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Run{Usage: u}, fmt.Errorf("claude (%s) timed out after %s", model, timeout)
		}
		return nil, Run{Usage: u}, fmt.Errorf("claude (%s): %w", model, err)
	}
	r := Run{Turns: res.Turns, Denials: len(res.Denials), Duration: time.Duration(res.DurationMS) * time.Millisecond,
		Cost: res.Cost, Usage: u}
	if res.Subtype == "error_max_budget_usd" {
		return nil, r, fmt.Errorf("claude (%s) hit its budget before answering ($%.2f spent)", model, res.Cost)
	}
	if res.IsError || len(res.StructuredOutput) == 0 || string(res.StructuredOutput) == "null" {
		return nil, r, fmt.Errorf("claude (%s) gave no structured result: %s", model, clip(res.Result, 400))
	}
	return res.StructuredOutput, r, nil
}
