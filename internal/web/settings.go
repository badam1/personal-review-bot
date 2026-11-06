package web

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/pipeline"
)

// settingsPage is /settings: config.env's settings, the reviewers and prompts, and the account they all act as.
// The settings form works without script; settings.js adds the unsaved-changes bar and the live previews.
type settingsPage struct {
	Token     string
	Msg, At   string             // what the last save did, and the section it is about ("" = the top)
	Err       string             // why the form wasn't saved; the page shows what was sent, to fix and save again
	File      string             // config.env, with ~ for the home directory
	F         map[string]setting // by name: ORG, THRESHOLD, …
	Polls     []choice           // Check GitHub every
	Parallel  []choice           // Reviews at a time
	Sevs      []choice           // Post on its own
	HighOn    bool               // high findings post on their own already, so it needs no confirmation
	Account   accountView
	Reviewers []reviewerRow
	Adding    reviewerRow // the drawer that adds a reviewer
	Prompts   []promptRow
	Context   string // context.md
	KitErr    string // the reviewers or prompts couldn't be read: the files to fix are named here
	Open      string // a drawer to show open: one whose save failed
	OpenErr   string // why it failed
	Usage     int    // % of the fuller of the plan's limits now (-1: not known yet)
	Mark      int    // where the bar marks it
	Zones     zones  // the bar of which models reviews use as the plan fills
	Moved     string // where a saved ADDR moves the page once the bot restarts ("": it is where it is)
	Restart   string // the command that restarts the bot on this computer ("" = unknown)
	Files     []fileRow
}

// setting is one config.env setting as the page shows it.
type setting struct {
	Key     string // its name in config.env and in the form: THRESHOLD
	Name    string // PERSONAL_REVIEW_BOT_ plus the key: what to set in the environment
	Value   string // as config.env would write it
	Default string // the same, for the default
	Shown   string // the default in words: "2 min", "any org"
	Env     bool   // set in the environment, which wins: the page shows it locked
	Changed bool   // differs from the default
}

type choice struct {
	Value, Label string
	On           bool
}

// zones split the plan's usage, in %, into where reviews use their usual models, cheaper ones and the cheapest.
type zones struct{ Usual, Cheaper, Cheapest int }

// drawer is what an editor opening over the page needs besides what it edits.
type drawer struct {
	Token string
	Open  bool   // shown open, without its link: its save failed
	Err   string // why
}

type reviewerRow struct {
	agents.ReviewerEdit
	drawer
	New     bool // the drawer that adds one
	Builtin bool
	Tag     string // Built-in, Customised or Yours
	Runs    string // which PRs it runs on: "every PR", or its path pattern
	ModelIs string // its model in words
	File    string // its file, from the config directory: agents/security.md
	Models  []choice
}

type promptRow struct {
	drawer
	Name, What, Text, File string
	Custom                 bool
}

type fileRow struct{ Name, Path, Full string }

// editable are the settings the page saves, in the order it shows them, with how a save names them. UPDATE_REPO is
// shown but not saved here: it decides where the code you run comes from, so it changes only with you at a shell.
var editable = []struct{ name, label string }{
	{"ORG", "Organisations"}, {"SKIP_DRAFTS", "Skip draft PRs"}, {"MAX_AGE_DAYS", "Leave quiet PRs alone after"},
	{"POLL_SECONDS", "Check GitHub every"}, {"PARALLEL", "Reviews at a time"}, {"AUTO_POST", "Post on its own"},
	{"THRESHOLD", "Certainty needed"}, {"APPROVE_TEXT", "Approval message"}, {"WATERMARK", "Signature"},
	{"SAVE_AT", "Cheaper models from"}, {"MIN_AT", "Cheapest from"}, {"AGENT_TIMEOUT_MINUTES", "Time limit per reviewer"},
	{"SHOW_LABELS", "PR labels to show"}, {"ADDR", "Address"}, {"UPDATE_CHECK", "Look for new releases"},
}

// shownDefault says each default in words, as the page writes it under the setting.
var shownDefault = map[string]string{
	"ORG": "any org", "SKIP_DRAFTS": "on", "MAX_AGE_DAYS": "14 days", "POLL_SECONDS": "2 min", "PARALLEL": "2",
	"AUTO_POST": "low and medium", "THRESHOLD": "80%", "APPROVE_TEXT": "LGTM :+1:", "WATERMARK": "🤖 personal-review-bot",
	"SAVE_AT": "75%", "MIN_AT": "90%", "AGENT_TIMEOUT_MINUTES": "20 minutes", "SHOW_LABELS": "risk/, auto-risk/",
	"ADDR": "127.0.0.1:12346", "UPDATE_CHECK": "on", "UPDATE_REPO": "badam1/personal-review-bot",
}

// promptWhat says what each prompt is for.
var promptWhat = map[string]string{
	"review":   "Every reviewer’s instructions, with its area and focus filled in",
	"verify":   "How the verifier checks each finding and rates its certainty",
	"design":   "How the design review judges the approach",
	"followup": "How a re-review checks what became of earlier findings",
	"summary":  "The two sentences at the top of a review",
}

// saved reads the settings as the environment and config.env have them now, which is what the page shows and what
// a save compares the form with. It reads the file afresh, so a change made there by hand shows too.
func (s *server) saved() config.Config { return config.LoadFrom(s.cfg().ConfigDir) }

func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	saved := s.saved()
	p := s.settingsPage(r.Context(), saved, saved.Values())
	p.Msg, p.At = r.URL.Query().Get("msg"), r.URL.Query().Get("at")
	s.renderSettings(w, http.StatusOK, p)
}

// renderSettings hands each drawer the token, and the one whose save failed its error, then renders the page.
func (s *server) renderSettings(w http.ResponseWriter, status int, p settingsPage) {
	mark := func(id string, d *drawer) {
		d.Token = p.Token
		if id == p.Open {
			d.Open, d.Err = true, p.OpenErr
		}
	}
	for i := range p.Reviewers {
		mark("rv-"+p.Reviewers[i].Name, &p.Reviewers[i].drawer)
	}
	mark("rv-new", &p.Adding.drawer)
	for i := range p.Prompts {
		mark("pr-"+p.Prompts[i].Name, &p.Prompts[i].drawer)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "settings", p); err != nil {
		s.d.Log("settings page: %v", err)
	}
}

// settingsPage builds the page for the settings in values: the saved ones, or a form that wasn't saved.
func (s *server) settingsPage(ctx context.Context, saved config.Config, values map[string]string) settingsPage {
	p := settingsPage{Token: s.token, File: tilde(saved.File()), F: map[string]setting{}, Usage: -1,
		Restart: restartCommand(), HighOn: slices.Contains(saved.AutoPost, "high")}
	for name, def := range config.Defaults {
		p.F[name] = setting{Key: name, Name: "PERSONAL_REVIEW_BOT_" + name, Value: values[name], Default: def, Shown: shownDefault[name],
			Env: config.FromEnv(name), Changed: values[name] != def}
	}
	p.Polls = choices(values["POLL_SECONDS"], []string{"60", "120", "300", "600"}, func(v string) string {
		n, _ := strconv.Atoi(v)
		if n%60 == 0 {
			return fmt.Sprintf("%d min", n/60)
		}
		return fmt.Sprintf("%d s", n)
	})
	p.Parallel = choices(values["PARALLEL"], []string{"1", "2", "3", "4"}, func(v string) string { return v })
	on := strings.Split(values["AUTO_POST"], ",")
	for _, sev := range []string{"low", "medium", "high"} {
		p.Sevs = append(p.Sevs, choice{Value: sev, Label: strings.ToUpper(sev[:1]) + sev[1:], On: slices.Contains(on, sev)})
	}
	if saved.WebAddr != s.listen && s.listen != "" {
		p.Moved = saved.WebAddr
	}
	if s.updates == nil { // the page alone, started by hand: restarting it is up to you
		p.Restart = ""
	}
	p.Account = s.account(ctx, config.SplitOrgs(values["ORG"])...)

	dir := saved.ConfigDir
	p.Adding = reviewerRow{ReviewerEdit: agents.ReviewerEdit{Enabled: true}, New: true, Tag: "Yours", Models: modelChoices("")}
	if kit, err := agents.Load(dir); err != nil {
		p.KitErr = err.Error()
	} else {
		for _, r := range kit.Reviewers {
			p.Reviewers = append(p.Reviewers, newReviewerRow(dir, r, true))
		}
		for _, r := range kit.Disabled {
			p.Reviewers = append(p.Reviewers, newReviewerRow(dir, r, false))
		}
		slices.SortStableFunc(p.Reviewers, func(a, b reviewerRow) int { return reviewerOrder(a) - reviewerOrder(b) })
		p.Context = kit.Context
	}
	for _, name := range agents.Prompts {
		text, custom, err := agents.PromptText(dir, name)
		if err != nil {
			p.KitErr = err.Error()
			continue
		}
		p.Prompts = append(p.Prompts, promptRow{Name: name, What: promptWhat[name], Text: text, Custom: custom,
			File: filepath.Join("prompts", name+".md")})
	}

	if u, ok := pipeline.LoadUsage(s.d.Store); ok {
		five, week := u.At(time.Now())
		p.Usage = int(max(five, week)*100 + .5)
		p.Mark = min(p.Usage, 99)
	}
	save, _ := strconv.Atoi(values["SAVE_AT"])
	cheapest, _ := strconv.Atoi(values["MIN_AT"])
	save = min(max(save, 0), 100)
	cheapest = min(max(cheapest, save), 100)
	p.Zones = zones{save, cheapest - save, 100 - cheapest}
	p.Files = []fileRow{{"Settings", tilde(saved.File()), saved.File()}, {"Data", tilde(saved.DBPath()), saved.DBPath()},
		{"Clones", tilde(saved.CacheDir), saved.CacheDir}}
	if log := about.Log; log != "" { // a file on macOS, a command on Linux
		full := log
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(log, "~/") {
			full = filepath.Join(home, log[2:])
		}
		p.Files = append(p.Files, fileRow{"Log", log, full})
	}
	return p
}

// choices are a segmented control's options; a value set by hand that isn't among them is offered too.
func choices(value string, values []string, label func(string) string) []choice {
	if !slices.Contains(values, value) {
		values = append(values, value)
		slices.SortFunc(values, func(a, b string) int { x, _ := strconv.Atoi(a); y, _ := strconv.Atoi(b); return x - y })
	}
	var out []choice
	for _, v := range values {
		out = append(out, choice{Value: v, Label: label(v), On: v == value})
	}
	return out
}

func modelChoices(model string) []choice {
	out := []choice{{"", "By PR size", model == ""}}
	for _, m := range []string{"haiku", "sonnet", "opus"} {
		out = append(out, choice{m, strings.ToUpper(m[:1]) + m[1:], model == m})
	}
	if model != "" && !slices.ContainsFunc(out, func(c choice) bool { return c.On }) {
		out = append(out, choice{model, model, true}) // a model pinned in the file by hand
	}
	return out
}

func newReviewerRow(dir string, r agents.Reviewer, enabled bool) reviewerRow {
	row := reviewerRow{ReviewerEdit: r.Edit(enabled), Builtin: agents.IsBuiltin(r.Name), Runs: "every PR",
		ModelIs: "Size of the PR", File: filepath.Join("agents", r.Name+".md"), Models: modelChoices(r.Model)}
	switch {
	case !row.Builtin:
		row.Tag = "Yours"
	case r.Source != "built-in":
		row.Tag = "Customised"
	default:
		row.Tag = "Built-in"
	}
	if r.Paths != nil {
		row.Runs = r.Paths.String()
	}
	if r.Model != "" {
		row.ModelIs = strings.ToUpper(r.Model[:1]) + r.Model[1:] + ", pinned"
	}
	return row
}

// reviewerOrder puts the built-in reviewers first, in their usual order, then yours.
func reviewerOrder(r reviewerRow) int {
	for i, name := range []string{"logic", "security", "integration", "infra"} {
		if r.Name == name {
			return i
		}
	}
	return 100
}

// restartCommand restarts the bot as install.sh set it up, for a setting that applies at start.
func restartCommand() string {
	switch runtime.GOOS {
	case "darwin":
		return "launchctl kickstart -k gui/$(id -u)/io.github.badam1.personal-review-bot"
	case "linux":
		return "systemctl --user restart personal-review-bot"
	}
	return ""
}

// formValue reads one setting from the form, as config.env would write it.
func formValue(name string, sent []string) string {
	last := ""
	if len(sent) > 0 {
		last = strings.TrimSpace(sent[len(sent)-1])
	}
	switch name {
	case "ORG": // several orgs, however they were typed, are written the way the page shows them back
		return strings.Join(config.SplitOrgs(last), ",")
	case "SKIP_DRAFTS", "UPDATE_CHECK": // a hidden 0 comes first, then the switch's 1 when it is on
		if slices.Contains(sent, "1") {
			return "1"
		}
		return "0"
	case "AUTO_POST": // a hidden empty value comes first, so a form with none ticked still says so
		var out []string
		for _, sev := range []string{"low", "medium", "high"} {
			if slices.Contains(sent, sev) {
				out = append(out, sev)
			}
		}
		return strings.Join(out, ",")
	case "SHOW_LABELS":
		var out []string
		for _, p := range strings.Split(last, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, ",")
	}
	return last
}

func (s *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	s.saving.Lock()
	defer s.saving.Unlock()
	saved := s.saved()
	cur := saved.Values()
	form := maps.Clone(cur)
	for _, e := range editable {
		if sent, ok := r.PostForm[e.name]; ok && !config.FromEnv(e.name) {
			form[e.name] = formValue(e.name, sent)
		}
	}

	var errs, changed []string
	for _, e := range editable {
		if form[e.name] == cur[e.name] {
			continue
		}
		changed = append(changed, e.label)
		if err := config.Check(e.name, form[e.name]); err != nil {
			errs = append(errs, e.label+": "+err.Error()+".")
		}
	}
	if save, err1 := strconv.Atoi(form["SAVE_AT"]); err1 == nil {
		if cheapest, err2 := strconv.Atoi(form["MIN_AT"]); err2 == nil && cheapest <= save {
			errs = append(errs, "Cheapest from must be above Cheaper models from.")
		}
	}
	if !slices.Contains(saved.AutoPost, "high") && slices.Contains(strings.Split(form["AUTO_POST"], ","), "high") &&
		r.PostFormValue("confirm_high") != "1" {
		errs = append(errs, "To post high findings without asking, tick the confirmation under Post on its own.")
	}

	dir := saved.ConfigDir
	kit, kitErr := agents.Load(dir)
	context, contextSent := r.PostForm["CONTEXT"]
	newContext := ""
	if contextSent {
		newContext = strings.TrimSpace(strings.ReplaceAll(context[0], "\r\n", "\n"))
	}
	if contextSent && kitErr == nil && newContext != kit.Context {
		changed = append(changed, "Notes about your organisation")
	}
	var toggles []agents.ReviewerEdit
	if kitErr == nil {
		on := r.PostForm["on"]
		for _, name := range r.PostForm["reviewer"] {
			want := slices.Contains(on, name)
			if rv, ok := kit.Reviewer(name); ok && !want {
				toggles = append(toggles, rv.Edit(false))
			} else if i := slices.IndexFunc(kit.Disabled, func(d agents.Reviewer) bool { return d.Name == name }); i >= 0 && want {
				toggles = append(toggles, kit.Disabled[i].Edit(true))
			}
		}
	}
	for _, t := range toggles {
		if t.Enabled {
			changed = append(changed, t.Name+" on")
		} else {
			changed = append(changed, t.Name+" off")
		}
	}

	if len(errs) > 0 {
		p := s.settingsPage(r.Context(), saved, form)
		p.Err = strings.Join(errs, " ")
		if contextSent {
			p.Context = newContext
		}
		s.renderSettings(w, http.StatusUnprocessableEntity, p)
		return
	}
	if len(changed) == 0 {
		http.Redirect(w, r, "/settings?msg="+url.QueryEscape("Nothing to save: it was all as it is."), http.StatusSeeOther)
		return
	}

	set, unset := map[string]string{}, []string{}
	for _, e := range editable {
		switch v := form[e.name]; {
		case v == cur[e.name]:
		case v == config.Defaults[e.name]:
			unset = append(unset, e.name)
		default:
			set[e.name] = v
		}
		if e.name == "AUTO_POST" && form[e.name] != cur[e.name] {
			unset = append(unset, "AUTO_SEVERITY") // the older setting AUTO_POST replaces
		}
	}
	fail := func(err error) {
		http.Error(w, "couldn't save the settings: "+err.Error(), http.StatusInternalServerError)
	}
	if len(set)+len(unset) > 0 {
		if err := config.Save(dir, set, unset); err != nil {
			fail(err)
			return
		}
	}
	if contextSent && kitErr == nil && newContext != kit.Context {
		if err := agents.SaveContext(dir, newContext); err != nil {
			fail(err)
			return
		}
	}
	for _, t := range toggles {
		if _, err := agents.SaveReviewer(dir, t); err != nil {
			fail(err)
			return
		}
	}
	s.apply()
	s.d.Log("settings saved from the page: %s", strings.Join(changed, ", "))

	msg := "Saved " + strings.Join(changed, ", ") + ". It applies from the next review."
	if form["ADDR"] != cur["ADDR"] {
		msg += " The page moves to " + strings.Replace("http://"+form["ADDR"], "127.0.0.1", "localhost", 1) +
			" once the bot restarts."
	}
	http.Redirect(w, r, "/settings?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// apply has the running bot use the settings as saved. What applies only at start stays as it started: the address
// the page listens on and the directories the database and clones are in.
func (s *server) apply() {
	running := s.cfg()
	next := config.LoadFrom(running.ConfigDir)
	next.WebAddr, next.StateDir, next.CacheDir = running.WebAddr, running.StateDir, running.CacheDir
	s.d.Live.Set(next)
}

// reviewerForm reads the reviewer drawer's form.
func reviewerForm(r *http.Request) agents.ReviewerEdit {
	return agents.ReviewerEdit{Name: strings.TrimSpace(r.PostFormValue("name")), Area: strings.TrimSpace(r.PostFormValue("area")),
		Paths: strings.TrimSpace(r.PostFormValue("paths")), Model: r.PostFormValue("model"), Focus: r.PostFormValue("focus"),
		Docs: r.PostFormValue("docs") == "1", Enabled: r.PostFormValue("enabled") == "1"}
}

func (s *server) saveReviewer(w http.ResponseWriter, r *http.Request) {
	s.saving.Lock()
	defer s.saving.Unlock()
	saved := s.saved()
	e := reviewerForm(r)
	adding := r.PostFormValue("new") == "1"
	var err error
	if adding {
		err = agents.NewReviewerName(saved.ConfigDir, e.Name)
	}
	path := ""
	if err == nil {
		path, err = agents.SaveReviewer(saved.ConfigDir, e)
	}
	if err != nil {
		p := s.settingsPage(r.Context(), saved, saved.Values())
		row := reviewerRow{ReviewerEdit: e, Builtin: agents.IsBuiltin(e.Name), Tag: "Yours", Models: modelChoices(e.Model),
			File: filepath.Join("agents", e.Name+".md")}
		if adding {
			row.New = true
			p.Adding, p.Open = row, "rv-new"
		} else {
			p.Open = "rv-" + e.Name
			for i := range p.Reviewers {
				if p.Reviewers[i].Name == e.Name {
					row.Tag, row.Runs, row.ModelIs = p.Reviewers[i].Tag, p.Reviewers[i].Runs, p.Reviewers[i].ModelIs
					p.Reviewers[i] = row
				}
			}
		}
		p.OpenErr = err.Error()
		s.renderSettings(w, http.StatusUnprocessableEntity, p)
		return
	}
	s.d.Log("settings: reviewer %s saved from the page", e.Name)
	msg := fmt.Sprintf("Saved %s as %s. It applies from the next review.", e.Name, tilde(path))
	if path == "" {
		msg = fmt.Sprintf("%s is as shipped again: nothing differed from the built-in reviewer.", e.Name)
	}
	http.Redirect(w, r, "/settings?at=reviewers&msg="+url.QueryEscape(msg)+"#reviewers", http.StatusSeeOther)
}

func (s *server) removeReviewer(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	s.saving.Lock()
	err := agents.RemoveReviewer(s.saved().ConfigDir, name)
	s.saving.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.d.Log("settings: reviewer %s reset from the page", name)
	msg := name + " is removed."
	if agents.IsBuiltin(name) {
		msg = name + " is the built-in reviewer again."
	}
	http.Redirect(w, r, "/settings?at=reviewers&msg="+url.QueryEscape(msg)+"#reviewers", http.StatusSeeOther)
}

func (s *server) savePrompt(w http.ResponseWriter, r *http.Request) {
	s.saving.Lock()
	defer s.saving.Unlock()
	saved := s.saved()
	name, text := r.PostFormValue("name"), r.PostFormValue("text")
	path, err := agents.SavePrompt(saved.ConfigDir, name, text)
	if err != nil {
		p := s.settingsPage(r.Context(), saved, saved.Values())
		for i := range p.Prompts {
			if p.Prompts[i].Name == name {
				p.Prompts[i].Text = strings.ReplaceAll(text, "\r\n", "\n")
			}
		}
		p.Open, p.OpenErr = "pr-"+name, err.Error()
		s.renderSettings(w, http.StatusUnprocessableEntity, p)
		return
	}
	s.d.Log("settings: prompt %s saved from the page", name)
	msg := fmt.Sprintf("Saved the %s prompt as %s. It applies from the next review.", name, tilde(path))
	if path == "" {
		msg = fmt.Sprintf("The %s prompt is the built-in one again: nothing differed from it.", name)
	}
	http.Redirect(w, r, "/settings?at=prompts&msg="+url.QueryEscape(msg)+"#prompts", http.StatusSeeOther)
}

func (s *server) resetPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	s.saving.Lock()
	err := agents.ResetPrompt(s.saved().ConfigDir, name)
	s.saving.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.d.Log("settings: prompt %s reset from the page", name)
	msg := fmt.Sprintf("The %s prompt is the built-in one again.", name)
	http.Redirect(w, r, "/settings?at=prompts&msg="+url.QueryEscape(msg)+"#prompts", http.StatusSeeOther)
}
