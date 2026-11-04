// Package pipeline reviews one PR end to end: fetch → size → check out → reviewers in parallel → verifier →
// post what's verified ≥ threshold → hand everything else (and every approval) to the confirmation page.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/diffmap"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/triage"
	"github.com/badam1/personal-review-bot/internal/workspace"
)

type Deps struct {
	Cfg   config.Config
	Store *store.Store
	GH    *gh.Client
	Log   func(format string, args ...any)
}

// Run reviews the PR behind review id. It records every outcome in the store and only returns an error for
// failures it could not record.
func Run(ctx context.Context, d Deps, id int64) (err error) {
	r, err := d.Store.Get(id)
	if err != nil {
		return err
	}
	tag := fmt.Sprintf("%s#%d", r.Repo, r.Number)
	stage := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		_ = d.Store.SetStage(id, s)
		d.Log("%s: %s", tag, s)
	}
	fail := func(e error) error {
		d.Log("%s: failed: %v", tag, e)
		return d.Store.Finish(id, store.Failed, "failed", "", e.Error())
	}
	defer func() {
		if p := recover(); p != nil {
			err = fail(fmt.Errorf("panic: %v", p))
		}
	}()

	stage("fetching PR")
	pr, err := d.GH.GetPR(r.Repo, r.Number)
	if err != nil {
		return fail(fmt.Errorf("fetch PR: %w", err))
	}
	files, err := d.GH.Files(r.Repo, r.Number)
	if err != nil {
		return fail(fmt.Errorf("fetch files: %w", err))
	}
	diff, err := d.GH.Diff(r.Repo, r.Number)
	if err != nil {
		return fail(fmt.Errorf("fetch diff: %w", err))
	}
	plan := triage.Make(pr, files)
	if u, ok := usageNow(ctx, d, stage); ok {
		used, window := u.Peak(time.Now())
		plan = plan.Within(used, window, float64(d.Cfg.SaveAt)/100, float64(d.Cfg.MinAt)/100)
		if plan.Saving != "" {
			d.Log("%s: %s at %s effort", tag, plan.Models(), plan.Effort)
		}
	}
	_ = d.Store.SetPlan(id, pr.Title, pr.Author, pr.HeadSHA, plan.Tier, plan.Models(), strings.Join(plan.Reasons, " · "),
		pr.Additions, pr.Deletions, pr.Files)
	r.HeadSHA, r.Title, r.Author = pr.HeadSHA, pr.Title, pr.Author

	stage("checking out %.7s", pr.HeadSHA)
	ws, err := workspace.Prepare(ctx, d.Cfg.CacheDir, r.Repo, r.Number, pr.HeadSHA)
	if err != nil {
		return fail(err)
	}
	defer ws.Cleanup()
	in := agents.Input{Repo: r.Repo, Number: r.Number, Title: pr.Title, Author: pr.Author, Body: pr.Body,
		Base: pr.BaseRef, Head: pr.HeadRef, Diff: diff, Dir: ws.Dir, HeadSHA: pr.HeadSHA, Effort: plan.Effort,
		Owner: d.GH.Login, Org: relatedOrg(d.Cfg.Org, r.Repo)}
	if prev, err := d.Store.PreviousComplete(r.Repo, r.Number, id); err == nil && prev != nil {
		in.PrevSHA = prev.HeadSHA
	}

	// The discussion so far: intentional choices, answers to earlier comments, what's resolved.
	stage("reading the conversation")
	if in.Conversation, err = d.GH.Conversation(r.Repo, r.Number, d.Cfg.Watermark); err != nil {
		d.Log("%s: conversation unavailable: %v", tag, err)
		in.Conversation = "(could not load the conversation)"
	}
	if prior, err := d.Store.PriorFindings(r.Repo, r.Number, id); err == nil {
		in.Prior = formatPrior(prior, "@"+d.GH.Login)
	}

	// Reviewers in parallel, with the design reviewer on a first review (or one you asked for), and a follow-up check
	// of what earlier reviews posted.
	withDesign := designNow(plan, in.PrevSHA, r.Source)
	earlier := unsettled(d, r.Repo, r.Number, id)
	withFollowup := followupNow(plan, earlier, pr.HeadSHA, r.Source)
	also := ""
	if withDesign {
		also = fmt.Sprintf(" + %s (%s)", agents.DesignAgent, plan.DesignModel)
	}
	if withFollowup {
		also += fmt.Sprintf(" + follow-up on %d earlier (%s)", len(earlier), plan.FollowupModel)
	}
	stage("reviewing: %s (%s)%s", strings.Join(plan.Agents, ", "), plan.AgentModel, also)
	type result struct {
		agent    string
		findings []agents.Finding
		run      agents.Run
		err      error
	}
	results := make(chan result, len(plan.Agents))
	var wg sync.WaitGroup
	for _, a := range plan.Agents {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			fs, run, err := agents.Review(ctx, a, plan.AgentModel, in, d.Cfg.AgentTimeout)
			_ = d.Store.AddCost(id, run.Cost)
			results <- result{a, fs, run, err}
		}(a)
	}
	var concerns []agents.Concern
	var designRun agents.Run
	var designErr error
	if withDesign {
		wg.Add(1)
		go func() {
			defer wg.Done()
			concerns, designRun, designErr = agents.Design(ctx, plan.DesignModel, in, d.Cfg.AgentTimeout)
			_ = d.Store.AddCost(id, designRun.Cost)
		}()
	}
	var followups []agents.Followup
	var followupRun agents.Run
	var followupErr error
	if withFollowup {
		wg.Add(1)
		go func() {
			defer wg.Done()
			followups, followupRun, followupErr = agents.CheckEarlier(ctx, plan.FollowupModel, in, earlier, d.Cfg.AgentTimeout)
			_ = d.Store.AddCost(id, followupRun.Cost)
		}()
	}
	wg.Wait()
	close(results)

	// What became of earlier findings: recorded on each finding, for the page, the timeline and the stats.
	stillOpen, followupFailed := 0, false
	if withFollowup {
		saveRun(d, id, agents.FollowupAgent, plan.FollowupModel, len(followups), followupRun, followupErr)
		if followupErr != nil {
			followupFailed = true
			d.Log("%s: follow-up check failed: %v", tag, followupErr)
		}
		asked := map[int64]bool{}
		for _, e := range earlier {
			asked[e.ID] = true
		}
		for _, f := range followups {
			if !asked[f.ID] {
				continue
			}
			_ = d.Store.SetFollowup(f.ID, f.Status, strings.TrimSpace(f.Note), id)
			if f.Status == store.FollowOpen || f.Status == store.FollowPartly {
				stillOpen++
			}
		}
	}

	// Design concerns are judgement: they skip the verifier and always wait for you.
	designFailed := false
	if withDesign {
		saveRun(d, id, agents.DesignAgent, plan.DesignModel, len(concerns), designRun, designErr)
		if designErr != nil {
			designFailed = true
			d.Log("%s: design review failed: %v", tag, designErr)
		}
		for _, c := range concerns {
			if _, err := d.Store.AddFinding(&store.Finding{ReviewID: id, Agent: agents.DesignAgent, Severity: c.Severity,
				Title: c.Title, Comment: c.Text(), AgentConfidence: c.Confidence, Confidence: c.Confidence,
				Status: store.Pending}); err != nil {
				return fail(err)
			}
		}
	}

	var cands []agents.Candidate
	var failed []string
	for res := range results {
		saveRun(d, id, res.agent, plan.AgentModel, len(res.findings), res.run, res.err)
		if res.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", res.agent, res.err))
			continue
		}
		for _, f := range res.findings {
			fid, err := d.Store.AddFinding(&store.Finding{ReviewID: id, Agent: res.agent, Path: f.Path, Line: f.Line,
				Severity: f.Severity, Title: f.Title, Comment: f.Comment, AgentConfidence: f.Confidence, Status: store.Candidate})
			if err != nil {
				return fail(err)
			}
			cands = append(cands, agents.NewCandidate(fid, res.agent, f))
		}
	}
	if len(failed) == len(plan.Agents) {
		return fail(fmt.Errorf("all reviewers failed: %s", strings.Join(failed, "; ")))
	}

	// Verifier: nothing is posted on a reviewer's word alone.
	summary := ""
	verdicts := map[int64]agents.Verdict{}
	if len(cands) > 0 {
		stage("verifying %d finding(s) (%s)", len(cands), plan.VerifierModel)
		vs, sum, run, err := agents.Verify(ctx, plan.VerifierModel, in, cands, d.Cfg.AgentTimeout)
		_ = d.Store.AddCost(id, run.Cost)
		saveRun(d, id, "verifier", plan.VerifierModel, len(vs), run, err)
		if err != nil {
			d.Log("%s: verifier failed, everything goes to the confirmation page: %v", tag, err)
		}
		summary = sum
		for _, v := range vs {
			verdicts[v.ID] = v
		}
	}

	// Decide per finding.
	lines, added := diffmap.FromFiles(files), diffmap.Added(files)
	all, err := d.Store.Findings(id)
	if err != nil {
		return fail(err)
	}
	var auto, pending []store.Finding
	for _, f := range all {
		if f.Agent == agents.DesignAgent {
			f.Verdict, f.Status = agents.DesignAgent, store.Pending
			pending = append(pending, f)
			if err := d.Store.UpdateFinding(&f); err != nil {
				return fail(err)
			}
			continue
		}
		v, ok := verdicts[f.ID]
		switch {
		case !ok: // unverified: never posted automatically
			f.Verdict, f.Confidence = "unverified", min(f.AgentConfidence, d.Cfg.Threshold-1)
		default:
			f.Verdict, f.Confidence = v.Verdict, v.Certainty
			if v.Severity != "" {
				f.Severity = v.Severity
			}
			if strings.TrimSpace(v.Comment) != "" {
				f.Comment = v.Comment
			}
			if v.Line > 0 {
				f.Line = v.Line
			}
		}
		f.Line, f.Inline = lines.Place(f.Path, f.Line)
		_ = d.Store.SetContext(f.ID, excerpt(ws.Dir, f.Path, f.Line, added[f.Path]))
		switch {
		case f.Verdict == "rejected":
			f.Status = store.Rejected
		case d.Cfg.AutoPosts(f.Verdict, f.Confidence, f.Severity):
			f.Status = store.AutoPosted
			auto = append(auto, f)
		default:
			f.Status = store.Pending
			pending = append(pending, f)
		}
		if err := d.Store.UpdateFinding(&f); err != nil {
			return fail(err)
		}
	}

	// Post the high-confidence findings as one COMMENT review (never an approval).
	posted := ""
	if len(auto) > 0 {
		if r.DryRun {
			for i := range auto {
				auto[i].Status = store.WouldPost
				_ = d.Store.UpdateFinding(&auto[i])
			}
			posted = fmt.Sprintf("dry run: would post %d", len(auto))
		} else {
			stage("posting %d comment(s)", len(auto))
			url, err := Post(d, r, "COMMENT", auto)
			if err != nil {
				return fail(fmt.Errorf("post review: %w", err))
			}
			for i := range auto {
				auto[i].PostedURL = url
				_ = d.Store.UpdateFinding(&auto[i])
			}
			posted = fmt.Sprintf("posted %d", len(auto))
		}
	}

	// This review replaces any older one still waiting for you; that one covered an older commit.
	if !r.DryRun {
		_ = d.Store.Supersede(r.Repo, r.Number, id)
	}

	decision, needsYou := outcome(posted, len(pending), len(failed), len(plan.Agents))
	if designFailed {
		decision += ", design review failed"
	}
	if stillOpen > 0 {
		decision += fmt.Sprintf(", %d earlier still open", stillOpen)
	}
	if followupFailed {
		decision += ", follow-up check failed"
	}
	if needsYou {
		if err := d.Store.Finish(id, store.AwaitingYou, decision, summary, strings.Join(failed, "; ")); err != nil {
			return err
		}
		if !r.DryRun {
			AskUser(d, r)
		}
		d.Log("%s: waiting for you: %s", tag, decision)
		return nil
	}
	d.Log("%s: done: %s", tag, decision)
	return d.Store.Finish(id, store.Done, decision, summary, strings.Join(failed, "; "))
}

// relatedOrg is where reviewers look for the repos a PR affects: the configured org, else the PR repo's owner.
func relatedOrg(org, repo string) string {
	if org != "" {
		return org
	}
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

// designNow says whether this review runs the design reviewer: on a PR worth it, on its first complete review or
// when you asked for the review; automatic re-reviews (re-request, new commits) leave the design review out.
func designNow(plan triage.Plan, prevSHA, source string) bool {
	return plan.Design && (prevSHA == "" || source == "manual")
}

// unsettled returns the findings posted on the PR's earlier reviews that no follow-up has settled yet (and you haven't
// marked wrong), each with the commit it was made on.
func unsettled(d Deps, repo string, number int, before int64) []agents.Earlier {
	fs, err := d.Store.EarlierPosted(repo, number, before)
	if err != nil {
		d.Log("%s#%d: earlier findings unavailable: %v", repo, number, err)
		return nil
	}
	commits := map[int64]string{}
	var out []agents.Earlier
	for _, f := range fs {
		if f.Wrong || f.Settled() {
			continue
		}
		if _, ok := commits[f.ReviewID]; !ok {
			if rev, err := d.Store.Get(f.ReviewID); err == nil {
				commits[f.ReviewID] = rev.HeadSHA
			}
		}
		out = append(out, agents.Earlier{ID: f.ID, Commit: commits[f.ReviewID], Path: f.Path, Line: f.Line, Title: f.Title,
			Comment: f.Comment})
	}
	return out
}

// followupNow says whether to check the earlier findings: there are some, and the PR has moved since at least one
// was made, or someone asked for this review (a re-request often answers comments in the thread instead of in code).
func followupNow(plan triage.Plan, earlier []agents.Earlier, head, source string) bool {
	if !plan.Followup || len(earlier) == 0 {
		return false
	}
	if source == "re-request" || source == "manual" {
		return true
	}
	for _, e := range earlier {
		if e.Commit != head {
			return true
		}
	}
	return false
}

// outcome sums up a review and says whether it goes to the confirmation page: for findings below the threshold, for
// a reviewer that failed (the result is incomplete, so the page offers a re-review instead of an approval), and when
// nothing was posted at all (approval is always yours).
func outcome(posted string, pending, failed, reviewers int) (decision string, needsYou bool) {
	var parts []string
	if posted != "" {
		parts = append(parts, posted)
	}
	switch {
	case pending > 0:
		parts = append(parts, fmt.Sprintf("%d need you", pending))
	case failed == 0 && posted == "":
		parts = append(parts, "no issues: approve?")
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d reviewers failed: re-review", failed, reviewers))
	}
	return strings.Join(parts, ", "), pending > 0 || failed > 0 || posted == ""
}

// saveRun records what one Claude run did, for the confirmation page, and the usage reading it brought.
func saveRun(d Deps, id int64, agent, model string, findings int, run agents.Run, err error) {
	sr := store.Run{ReviewID: id, Agent: agent, Model: model, Findings: findings, Turns: run.Turns, Denials: run.Denials,
		Duration: run.Duration, CostUSD: run.Cost, Checked: run.Checked}
	if err != nil {
		sr.Error = err.Error()
	}
	_ = d.Store.AddRun(&sr)
	if run.Usage != nil {
		saveUsage(d.Store, *run.Usage)
	}
}

// usageFresh is how long a usage reading is trusted: your own Claude use fills the same limits.
const usageFresh = 10 * time.Minute

// usageNow is how full the Claude plan's limits are: the last run's reading while it is fresh, else a new one.
func usageNow(ctx context.Context, d Deps, stage func(string, ...any)) (agents.Usage, bool) {
	if u, ok := LoadUsage(d.Store); ok && time.Since(u.Seen) < usageFresh {
		return u, true
	}
	stage("checking usage limits")
	u, err := agents.CheckUsage(ctx)
	if err != nil {
		d.Log("usage limits unknown, reviewing as planned: %v", err)
		return agents.Usage{}, false
	}
	saveUsage(d.Store, u)
	return u, true
}

// LoadUsage returns the latest usage reading (false if there is none yet).
func LoadUsage(st *store.Store) (agents.Usage, bool) {
	var u agents.Usage
	if json.Unmarshal([]byte(st.Meta("usage")), &u) != nil || u.Seen.IsZero() {
		return agents.Usage{}, false
	}
	return u, true
}

// saveUsage keeps u unless a newer reading is already stored (runs finish out of order).
func saveUsage(st *store.Store, u agents.Usage) {
	if last, ok := LoadUsage(st); ok && last.Seen.After(u.Seen) {
		return
	}
	if js, err := json.Marshal(u); err == nil {
		_ = st.SetMeta("usage", string(js))
	}
}

// Post submits findings as one review. Every comment carries the watermark and its confidence; findings whose
// line is not in the diff go into the review body instead of inline.
func Post(d Deps, r *store.Review, event string, fs []store.Finding) (string, error) {
	body, comments := reviewBody(d.Cfg, fs)
	// The approval text is said once per PR: a re-approval after new commits goes without it.
	if event == "APPROVE" && d.Cfg.ApproveText != "" {
		said, err := d.GH.ApprovedWith(r.Repo, r.Number, d.Cfg.ApproveText)
		if err != nil {
			d.Log("%s#%d: couldn't read earlier approvals, saying %q anyway: %v", r.Repo, r.Number, d.Cfg.ApproveText, err)
		}
		if !said {
			body = append([]string{d.Cfg.ApproveText}, body...)
		}
	}
	return d.GH.PostReview(r.Repo, r.Number, r.HeadSHA, event, strings.Join(body, "\n\n"), comments)
}

// reviewBody lays the findings out as a review: design concerns first (they're about the whole PR), then findings
// by confidence, inline where their line is in the diff.
func reviewBody(cfg config.Config, fs []store.Finding) ([]string, []gh.Comment) {
	sort.SliceStable(fs, func(i, j int) bool {
		if a, b := fs[i].Agent == agents.DesignAgent, fs[j].Agent == agents.DesignAgent; a != b {
			return a
		}
		return fs[i].Confidence > fs[j].Confidence
	})
	var comments []gh.Comment
	var body []string
	for _, f := range fs {
		if f.Agent == agents.DesignAgent {
			body = append(body, fmt.Sprintf("**Design: %s**\n\n%s\n\n%s", f.Title, strings.TrimSpace(f.Comment), cfg.DesignFooter(f.Confidence)))
			continue
		}
		text := strings.TrimSpace(f.Comment) + "\n\n" + cfg.Footer(f.Confidence)
		if f.Inline {
			comments = append(comments, gh.Comment{Path: f.Path, Line: f.Line, Side: "RIGHT", Body: text})
		} else {
			body = append(body, fmt.Sprintf("**`%s:%d`**: %s", f.Path, f.Line, text))
		}
	}
	return body, comments
}

var followupLabels = map[string]string{
	store.FollowFixed:    "fixed",
	store.FollowPartly:   "partly fixed",
	store.FollowOpen:     "still open",
	store.FollowMoot:     "no longer applies",
	store.FollowDeclined: "the author declined",
}

// formatPrior lists earlier findings on the PR and what became of them, for re-reviews. owner is who decides
// ("@login"), as the prompts name them.
func formatPrior(fs []store.Finding, owner string) string {
	fates := map[string]string{
		store.AutoPosted: "posted by the bot",
		store.Posted:     "posted by " + owner,
		store.Discarded:  owner + " chose NOT to post",
		store.Pending:    "never decided",
	}
	var b strings.Builder
	for _, f := range fs {
		fate := fates[f.Status]
		if f.Wrong {
			fate += ", then " + owner + " marked it WRONG"
		}
		if label := followupLabels[f.Followup]; label != "" {
			fate += "; last follow-up: " + label
		}
		if f.Agent == agents.DesignAgent {
			fmt.Fprintf(&b, "- [%s] design concern: %s\n", fate, f.Title)
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s:%d %s (%d%%)\n", fate, f.Path, f.Line, f.Title, f.Confidence)
	}
	return b.String()
}

// excerptLines is how far around a finding's line its code excerpt reaches.
const excerptLines = 3

// excerpt is the code around line in the checkout, as "line\tmark\ttext" rows (mark "+" for a line the PR adds), or ""
// when the file or line isn't there. The path comes from a reviewer, so it is kept inside the checkout.
func excerpt(dir, path string, line int, added map[int]bool) string {
	full := filepath.Join(dir, filepath.Clean("/"+path))
	if path == "" || line < 1 || !strings.HasPrefix(full, filepath.Clean(dir)+string(filepath.Separator)) {
		return ""
	}
	if st, err := os.Stat(full); err != nil || st.Size() > 2<<20 {
		return ""
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ""
	}
	src := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if line > len(src) {
		return ""
	}
	var rows []string
	for n := max(1, line-excerptLines); n <= min(len(src), line+excerptLines); n++ {
		mark := " "
		if added[n] {
			mark = "+"
		}
		text := strings.TrimRight(src[n-1], "\r")
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		rows = append(rows, fmt.Sprintf("%d\t%s\t%s", n, mark, text))
	}
	return strings.Join(rows, "\n")
}

// AskUser sends a desktop notification; the review waits under "Needs you" on the page and in the TUI. It opens no
// browser tab (one per review piled up). On macOS, through terminal-notifier a click opens the review; where that
// isn't installed or macOS doesn't allow its notifications yet, a plain notification goes out instead. On Linux it
// uses notify-send when it is there. Elsewhere it sends nothing.
func AskUser(d Deps, r *store.Review) {
	title, msg := "personal-review-bot: needs you", fmt.Sprintf("%s#%d: %s", r.Repo, r.Number, r.Title)
	url := fmt.Sprintf("%s/r/%d", d.Cfg.WebURL(), r.ID)
	go func() {
		switch runtime.GOOS {
		case "darwin":
			if tn, err := exec.LookPath("terminal-notifier"); err == nil {
				err := exec.Command(tn, "-title", title, "-message", msg, "-open", url,
					"-group", fmt.Sprintf("personal-review-bot-%d", r.ID)).Run()
				if err == nil {
					return
				}
				d.Log("terminal-notifier failed (are its notifications allowed?), sending a plain one: %v", err)
			}
			_ = exec.Command("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "%s"`,
				strings.NewReplacer(`"`, "'", `\`, "").Replace(msg), title)).Run()
		case "linux":
			if ns, err := exec.LookPath("notify-send"); err == nil {
				_ = exec.Command(ns, "--app-name=personal-review-bot", title, msg+"\n"+url).Run()
			}
		}
	}()
}
