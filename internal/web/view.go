package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/update"
)

// header is the bar on every page, from the templates' header function. Serve points it at its server; tests point
// it at theirs. Nil (the zero value) draws the bar without the bot's state.
var header func() *headerView

// headerView is what the bar says: what the bot is doing, whether it watches GitHub, the plan's usage and its version.
type headerView struct {
	Running  int           // reviews queued or running
	Lead     *store.Review // the one the working pill names: the first running, else the first queued
	Watching bool          // the daemon polled GitHub recently
	Daemon   bool          // the daemon serves the page (not `personal-review-bot page` alone)
	LastPoll time.Time
	Usage    *usageView
	Version  versionView
	Token    string
}

type usageView struct{ FiveHour, Week int } // percent of the plan's 5-hour and weekly limits

// versionView is the version button and its menu: up to date, a release to install, an update under way or just done.
type versionView struct {
	Current    string
	State      string // what an update under way is doing, or why it failed
	Busy       bool
	Failed     bool
	Target     string // the version an update installs
	Steps      []stepView
	Percent    int
	Available  *update.Release
	Released   string // when Available was published, e.g. "today"
	Checked    time.Time
	CheckErr   string
	Done       *update.Done
	CanInstall bool
}

// stepView is one step of a running review or an update: done, the current one, or still to come.
type stepView struct {
	Label string
	State string // done | now | todo
}

// badge is a short label with the kind that colours it.
type badge struct {
	Text string
	Kind string // decide | open | design | approve | ok | post | run | need | fail | skip
}

func (s *server) headerView() *headerView {
	st := s.d.Store
	h := &headerView{Daemon: s.updates != nil, Token: s.token}
	if active, err := st.Active(); err == nil {
		h.Running = len(active)
		for i := range active {
			if h.Lead == nil || active[i].Status == store.Running && h.Lead.Status != store.Running {
				h.Lead = &active[i]
			}
		}
	}
	h.LastPoll, _ = time.Parse(time.RFC3339, st.Meta("last_poll"))
	interval := s.d.Cfg.PollInterval
	if interval == 0 {
		interval = 2 * time.Minute
	}
	h.Watching = !h.LastPoll.IsZero() && time.Since(h.LastPoll) < 2*interval+30*time.Second
	if u, ok := pipeline.LoadUsage(st); ok {
		five, week := u.At(time.Now())
		h.Usage = &usageView{FiveHour: int(five*100 + .5), Week: int(week*100 + .5)}
	}
	h.Version = s.versionView()
	return h
}

func (s *server) versionView() versionView {
	st := s.d.Store
	v := versionView{Current: s.version, State: update.State(st), CanInstall: s.updates != nil}
	v.Busy = update.Busy(v.State)
	v.Failed = v.State != "" && !v.Busy
	check := update.Load(st)
	v.Checked, v.CheckErr = check.Checked, check.Err
	if rel, ok := update.Available(check, s.version); ok {
		v.Available = &rel
		v.Released = day(rel.Published)
	}
	if v.Busy {
		if check.Latest != nil {
			v.Target = check.Latest.Version
		}
		v.Steps, v.Percent = updateSteps(v.State)
	}
	if d, ok := update.LastDone(st, time.Now()); ok && d.To == s.version {
		v.Done = &d
	}
	return v
}

// updateSteps lays an update's state out as its steps, and how far along it is.
func updateSteps(state string) ([]stepView, int) {
	steps := []struct{ todo, done, prefix string }{
		{"Download", "Downloaded", "downloading"},
		{"Check the checksum and build attestation", "Checksum and build attestation verified", "checking"},
		{"Install", "Installed", "installing"},
		{"Wait for running reviews", "Running reviews finished", ""}, // "v… installed: restarting once …"
		{"Restart", "Restarted", "restarting"},
	}
	at := 0
	switch {
	case strings.Contains(state, "installed: restarting"):
		at = 3
	default:
		for i, s := range steps {
			if s.prefix != "" && strings.HasPrefix(state, s.prefix) {
				at = i
			}
		}
	}
	out := make([]stepView, len(steps))
	for i, s := range steps {
		switch {
		case i < at:
			out[i] = stepView{s.done, "done"}
		case i == at:
			out[i] = stepView{stepLabel(state), "now"}
		default:
			out[i] = stepView{s.todo, "todo"}
		}
	}
	return out, (at*100 + 50) / len(steps)
}

// stepLabel is an update's state as its current step says it: "v0.2.0 installed: restarting once …" reads as
// "Restarting once …" under the Install step that's already ticked (update.js does the same).
func stepLabel(state string) string {
	if i := strings.Index(state, ": "); i >= 0 && strings.HasPrefix(state, "v") {
		state = state[i+2:]
	}
	return capitalize(state)
}

// reviewSteps lays a queued or running review's stage out as the four steps it goes through.
func reviewSteps(r store.Review) []stepView {
	labels := []string{"Fetch and check out", "Review", "Verify", "Post"}
	at := reviewStep(r.Stage)
	if r.Status == store.Queued {
		at = -1
	}
	out := make([]stepView, len(labels))
	for i, l := range labels {
		switch {
		case i < at:
			out[i] = stepView{l, "done"}
		case i == at:
			out[i] = stepView{l, "now"}
		default:
			out[i] = stepView{l, "todo"}
		}
	}
	return out
}

// reviewStep is the step a stage belongs to (running.js keeps the same mapping live).
func reviewStep(stage string) int {
	switch {
	case strings.HasPrefix(stage, "posting"):
		return 3
	case strings.HasPrefix(stage, "verifying"):
		return 2
	case strings.HasPrefix(stage, "reviewing"):
		return 1
	}
	return 0
}

// home is the section a review's page sits under, in the bar and its breadcrumb: Reviews while it still needs
// something (it waits for you, is queued or running, or failed and wasn't retried), History once it's over. A PR's
// timeline sits where its latest review does.
func home(r store.Review, retried bool) string {
	switch r.Status {
	case store.AwaitingYou, store.Queued, store.Running:
		return "reviews"
	case store.Failed:
		if !retried {
			return "reviews"
		}
	}
	return "history"
}

// outcome says how a review ended up, for the lists.
func outcome(r store.Review) badge {
	d := strings.ToLower(r.Decision)
	switch r.Status {
	case store.Queued, store.Running:
		return badge{"Running", "run"}
	case store.AwaitingYou:
		return badge{"Needs you", "need"}
	case store.Failed:
		return badge{"Failed", "fail"}
	case store.Ignored:
		return badge{"Skipped", "skip"}
	}
	switch {
	case strings.Contains(d, "superseded"):
		return badge{"Superseded", "skip"}
	case strings.Contains(d, "approved"):
		return badge{"Approved", "ok"}
	case strings.Contains(d, "posted"), strings.Contains(d, "reminded"):
		return badge{"Posted", "post"}
	case strings.Contains(d, "skipped"), strings.Contains(d, "ignored"):
		return badge{"Skipped", "skip"}
	}
	return badge{"Done", "post"}
}

// outcomeNote is the detail beside a review's outcome: its decision, or for a failure its error.
func outcomeNote(r store.Review) string {
	if r.Status == store.Failed {
		return clipText(r.Error, 60)
	}
	if r.Status == store.Queued || r.Status == store.Running {
		return r.Stage
	}
	return r.Decision
}

// waitingBadges says what a review waiting for you holds: findings to decide, design concerns, earlier findings still
// open, or nothing new (approve?).
func waitingBadges(fs []store.Finding, earlier []store.Finding) []badge {
	pending, design, open := 0, 0, 0
	for _, f := range fs {
		if f.Status != store.Pending {
			continue
		}
		if f.Agent == "design" {
			design++
		} else {
			pending++
		}
	}
	for _, f := range earlier {
		if pipeline.Remindable(f) {
			open++
		}
	}
	var out []badge
	if pending > 0 {
		out = append(out, badge{fmt.Sprintf("%d to decide", pending), "decide"})
	}
	if design > 0 {
		out = append(out, badge{plural(design, "design concern", "design concerns"), "design"})
	}
	if open > 0 {
		out = append(out, badge{fmt.Sprintf("%d earlier still open", open), "open"})
	}
	if pending == 0 && design == 0 {
		out = append(out, badge{"Nothing new: approve?", "approve"})
	}
	return out
}

// earlierBadges counts earlier findings by what the follow-up found, for the section's heading.
func earlierBadges(fs []store.Finding) []badge {
	order := []struct{ key, word, kind string }{
		{store.FollowFixed, "fixed", "ok"}, {store.FollowPartly, "partly fixed", "open"}, {store.FollowOpen, "still open", "open"},
		{store.FollowMoot, "no longer apply", "skip"}, {store.FollowDeclined, "declined", "skip"}, {"", "not checked yet", "skip"},
	}
	counts := map[string]int{}
	for _, f := range fs {
		if !f.Wrong {
			counts[f.Followup]++
		}
	}
	var out []badge
	for _, o := range order {
		if counts[o.key] > 0 {
			out = append(out, badge{fmt.Sprintf("%d %s", counts[o.key], o.word), o.kind})
		}
	}
	return out
}

// followupKind colours an earlier finding by what the follow-up found.
func followupKind(f store.Finding) string {
	switch f.Followup {
	case store.FollowFixed:
		return "ok"
	case store.FollowOpen, store.FollowPartly:
		return "open"
	}
	return "skip"
}

// sizeKind colours a review's size label.
func sizeKind(tier string) string {
	switch tier {
	case "small", "medium", "large", "deep":
		return tier
	}
	return "none"
}

// certKind colours a certainty: high enough to post, middling, or low.
func certKind(confidence int) string {
	switch {
	case confidence >= 80:
		return "hi"
	case confidence >= 60:
		return "mid"
	}
	return "lo"
}

// day is a date as the version menu says it: today, yesterday, or Jan 2.
func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "today"
	case !t.Before(today.AddDate(0, 0, -1)):
		return "yesterday"
	}
	return t.Local().Format("Jan 2")
}

func took(r store.Review) string {
	if r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		return ""
	}
	return r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
