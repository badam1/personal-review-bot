package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
)

// newSettingsSite is a site with its own config directory, and the account seeded into the cache so the page
// renders without asking GitHub, which is also what a second load within the hour does.
func newSettingsSite(t *testing.T) (*site, string) {
	t.Helper()
	s := newSite(t)
	dir := t.TempDir()
	cfg := config.LoadFrom(dir)
	cfg.StateDir, cfg.CacheDir = t.TempDir(), t.TempDir()
	s.srv.d.Cfg, s.srv.d.Live, s.srv.listen = cfg, config.NewLive(cfg), cfg.WebAddr
	s.srv.accounts = &accountCache{at: time.Now(), acc: gh.Account{Login: "octocat", Name: "Mona",
		HTMLURL: "https://github.com/octocat", Orgs: []string{"acme", "umbrella"}}}
	return s, dir
}

// response GETs path and returns the whole response, headers included.
func (s *site) response(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:12346"+path, nil))
	return rec
}

// postWithoutToken POSTs a form the way another website could: without the page's token.
func (s *site) postWithoutToken(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost:12346"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// The Account section says who every comment and approval goes out as, with the watched org marked among the ones
// the account belongs to. The picture is served by the bot, so the page never points the browser at GitHub for it.
func TestSettingsAccount(t *testing.T) {
	s, _ := newSettingsSite(t)
	if err := config.Save(s.srv.cfg().ConfigDir, map[string]string{"ORG": "acme"}, nil); err != nil {
		t.Fatal(err)
	}
	code, page := s.get("/settings")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want(t, page, "Acting as @octocat", "Mona", "acme · watched", `<span class="org">umbrella</span>`,
		`href="https://github.com/octocat"`, `<span class="face" aria-hidden="true">O</span>`,
		`<a class="chip setpill" href="/settings" aria-current="page">`)
	if strings.Contains(page, "avatars.githubusercontent.com") || strings.Contains(page, "/profile") {
		t.Error("the page points at GitHub for the picture, or at the old profile page")
	}

	s.srv.accounts.pic, s.srv.accounts.picType = []byte("\x89PNG"), "image/png"
	_, page = s.get("/settings")
	want(t, page, `<img class="face" src="/avatar"`)
	rec := s.response("/avatar")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("/avatar: %d %v", rec.Code, rec.Header())
	}

	s.srv.accounts.acc.Orgs = []string{"umbrella"}
	_, page = s.get("/settings")
	want(t, page, "This account isn't a member of acme")
}

// Without GitHub the page still has every setting, and names the account from start-up.
func TestSettingsWithoutGitHub(t *testing.T) {
	s, _ := newSettingsSite(t)
	s.srv.accounts = &accountCache{at: time.Now(), err: errors.New("no network")}
	code, page := s.get("/settings")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want(t, page, "Acting as @octocat", "GitHub couldn't be asked just now: no network", `name="THRESHOLD"`)
}

func TestProfileMovedToSettings(t *testing.T) {
	s, _ := newSettingsSite(t)
	rec := s.response("/profile")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/settings#account" {
		t.Errorf("/profile: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A save writes what changed to config.env, context.md and the reviewers' files, and the running bot uses it from
// the next review on: no restart.
func TestSaveSettings(t *testing.T) {
	s, dir := newSettingsSite(t)
	form := url.Values{"ORG": {"acme"}, "THRESHOLD": {"85"}, "POLL_SECONDS": {"300"}, "SKIP_DRAFTS": {"0"},
		"AUTO_POST": {"", "low"}, "WATERMARK": {"🤖 acme bot"}, "APPROVE_TEXT": {"LGTM :+1:"}, "PARALLEL": {"2"},
		"SHOW_LABELS": {"risk/, size/"}, "UPDATE_CHECK": {"0", "1"}, "CONTEXT": {"ADRs live in acme/adr.\r\n"},
		"reviewer": {"logic", "security"}, "on": {"logic"}}
	rec := s.post("/settings", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "msg=Saved") || !strings.Contains(loc, "security+off") {
		t.Errorf("redirected to %q", loc)
	}
	env := fileText(t, filepath.Join(dir, "config.env"))
	for _, line := range []string{"ORG=acme\n", "THRESHOLD=85\n", "POLL_SECONDS=300\n", "SKIP_DRAFTS=0\n", "AUTO_POST=low\n",
		"WATERMARK=\"🤖 acme bot\"\n", "SHOW_LABELS=risk/,size/\n"} {
		if !strings.Contains(env, line) {
			t.Errorf("config.env is missing %q:\n%s", line, env)
		}
	}
	for _, unchanged := range []string{"APPROVE_TEXT", "PARALLEL", "UPDATE_CHECK"} {
		if strings.Contains(env, unchanged) {
			t.Errorf("%s was written though it didn't change:\n%s", unchanged, env)
		}
	}
	cfg := s.srv.cfg()
	if strings.Join(cfg.Orgs, ",") != "acme" || cfg.Threshold != 85 || cfg.PollInterval != 5*time.Minute || cfg.SkipDrafts ||
		strings.Join(cfg.AutoPost, ",") != "low" || cfg.Watermark != "🤖 acme bot" {
		t.Errorf("the running config: %+v", cfg)
	}
	if got := fileText(t, filepath.Join(dir, "context.md")); got != "ADRs live in acme/adr.\n" {
		t.Errorf("context.md: %q", got)
	}
	if got := fileText(t, agents.ReviewerPath(dir, "security")); got != "---\nenabled: false\n---\n" {
		t.Errorf("agents/security.md: %q", got)
	}

	_, page := s.get("/settings")
	want(t, page, `value="acme"`, `<span class="pill changed">Changed</span>`, `data-reset="THRESHOLD" data-value="80"`,
		`aria-label="security on"><span class="track">`, "ADRs live in acme/adr.</textarea>")

	// Back to a default: the line goes, and with it the Changed mark.
	rec = s.post("/settings", url.Values{"THRESHOLD": {"80"}, "reviewer": {"security"}, "on": {"security"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if env := fileText(t, filepath.Join(dir, "config.env")); strings.Contains(env, "THRESHOLD") {
		t.Errorf("THRESHOLD is still in config.env:\n%s", env)
	}
	if _, err := os.Stat(agents.ReviewerPath(dir, "security")); !os.IsNotExist(err) {
		t.Errorf("security is back on, but its file is still there: %v", err)
	}
	if rec := s.post("/settings", url.Values{"THRESHOLD": {"80"}}); !strings.Contains(rec.Header().Get("Location"), "Nothing+to+save") {
		t.Errorf("an unchanged form: %q", rec.Header().Get("Location"))
	}
}

// A form that can't be saved comes back with what was sent and why, and nothing is written.
func TestSaveSettingsRejects(t *testing.T) {
	s, dir := newSettingsSite(t)
	for _, c := range []struct {
		form url.Values
		why  string
	}{
		{url.Values{"SAVE_AT": {"95"}, "MIN_AT": {"90"}}, "Cheapest from must be above Cheaper models from"},
		{url.Values{"ADDR": {"0.0.0.0:12346"}}, "Address: must be 127.0.0.1"},
		{url.Values{"ORG": {"acme/infra"}}, "Organisations: must be a GitHub organisation"},
		{url.Values{"AUTO_POST": {"", "low", "medium", "high"}}, "tick the confirmation under Post on its own"},
		{url.Values{"WATERMARK": {"<img src=x>"}}, "Signature: must be 1 to 80 characters, without &lt; or &gt;"},
	} {
		rec := s.post("/settings", c.form)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v: status %d", c.form, rec.Code)
			continue
		}
		want(t, rec.Body.String(), "Not saved.", c.why, "data-unsaved")
	}
	if rec := s.post("/settings", url.Values{"SAVE_AT": {"95"}, "MIN_AT": {"90"}}); !strings.Contains(rec.Body.String(), `name="SAVE_AT" value="95"`) {
		t.Error("the page lost what was sent")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.env")); !os.IsNotExist(err) {
		t.Errorf("config.env was written: %q", fileText(t, filepath.Join(dir, "config.env")))
	}

	// Ticked, high findings post on their own.
	rec := s.post("/settings", url.Values{"AUTO_POST": {"", "low", "medium", "high"}, "confirm_high": {"1"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(fileText(t, filepath.Join(dir, "config.env")), "AUTO_POST=low,medium,high") {
		t.Errorf("confirmed: status %d, config.env %q", rec.Code, fileText(t, filepath.Join(dir, "config.env")))
	}
	_, page := s.get("/settings")
	if strings.Contains(page, `name="confirm_high"`) {
		t.Error("asked to confirm high findings that already post on their own")
	}
}

// A setting in the environment wins over the page: it shows locked, and a save leaves it alone.
func TestSettingsFromEnvironment(t *testing.T) {
	t.Setenv("PERSONAL_REVIEW_BOT_THRESHOLD", "90")
	s, dir := newSettingsSite(t)
	_, page := s.get("/settings")
	want(t, page, `name="THRESHOLD" min="50" max="100" step="5" value="90" disabled`,
		"Set by <code>PERSONAL_REVIEW_BOT_THRESHOLD</code> in the environment")
	if rec := s.post("/settings", url.Values{"THRESHOLD": {"70"}, "ORG": {"acme"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	if env := fileText(t, filepath.Join(dir, "config.env")); env != "ORG=acme\n" {
		t.Errorf("config.env: %q", env)
	}
}

// A new address waits for a restart: the page says where it will be, and the running page stays where it is.
func TestSettingsAddressNeedsRestart(t *testing.T) {
	s, _ := newSettingsSite(t)
	rec := s.post("/settings", url.Values{"ADDR": {"127.0.0.1:12399"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "localhost%3A12399+once+the+bot+restarts") {
		t.Errorf("redirected to %q", loc)
	}
	if s.srv.cfg().WebAddr != "127.0.0.1:12346" {
		t.Errorf("the running page moved to %s before a restart", s.srv.cfg().WebAddr)
	}
	_, page := s.get("/settings")
	want(t, page, `value="127.0.0.1:12399"`, "Saved: the page moves to 127.0.0.1:12399 once the bot restarts.")
}

// The reviewer editor adds, changes and removes a reviewer's file; one that wouldn't load isn't written, and the
// editor comes back open with what was typed.
func TestSettingsReviewerEditor(t *testing.T) {
	s, dir := newSettingsSite(t)
	_, page := s.get("/settings")
	want(t, page, `id="rv-security"`, `id="rv-new"`, `href="#rv-security"`, `<b>logic</b><span class="pill sm">Built-in</span>`)

	add := url.Values{"new": {"1"}, "name": {"cost"}, "area": {"cloud cost"}, "paths": {"("}, "focus": {"Instance sizes."},
		"enabled": {"1"}, "model": {""}}
	rec := s.post("/settings/reviewer", add)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a bad pattern: status %d", rec.Code)
	}
	want(t, rec.Body.String(), `<div class="drawer open" id="rv-new">`, "paths: error parsing regexp", `value="cost"`)

	add.Set("paths", `\.tf$`)
	if rec := s.post("/settings/reviewer", add); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := fileText(t, agents.ReviewerPath(dir, "cost")); got != "---\narea: cloud cost\npaths: \\.tf$\n---\nInstance sizes.\n" {
		t.Errorf("agents/cost.md: %q", got)
	}
	if rec := s.post("/settings/reviewer", add); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "there is already a reviewer called cost") {
		t.Errorf("adding it twice: %d", rec.Code)
	}
	_, page = s.get("/settings")
	want(t, page, `<b>cost</b><span class="pill sm yours">Yours</span>`, `Remove reviewer`)

	sec, _ := agents.BuiltinReviewer("security")
	edit := url.Values{"name": {"security"}, "area": {sec.Area}, "paths": {""}, "focus": {sec.Focus}, "enabled": {"1"}, "model": {"opus"}}
	if rec := s.post("/settings/reviewer", edit); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	_, page = s.get("/settings")
	want(t, page, "Opus, pinned", `<b>security</b><span class="pill sm changed">Customised</span>`, "Reset to built-in")

	for _, name := range []string{"security", "cost"} {
		if rec := s.post("/settings/reviewer/remove", url.Values{"name": {name}}); rec.Code != http.StatusSeeOther {
			t.Errorf("remove %s: %d", name, rec.Code)
		}
		if _, err := os.Stat(agents.ReviewerPath(dir, name)); !os.IsNotExist(err) {
			t.Errorf("agents/%s.md is still there", name)
		}
	}
	if rec := s.post("/settings/reviewer/remove", url.Values{"name": {"../config"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a path for a name: %d", rec.Code)
	}
}

// The prompt editor saves a prompt only once it works as a template, and Reset goes back to the built-in one.
func TestSettingsPromptEditor(t *testing.T) {
	s, dir := newSettingsSite(t)
	rec := s.post("/settings/prompt", url.Values{"name": {"summary"}, "text": {"Say {{.Nope}}"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a broken template: status %d", rec.Code)
	}
	want(t, rec.Body.String(), `<div class="drawer open" id="pr-summary">`, "doesn&#39;t work as a template", "Say {{.Nope}}</textarea>")

	if rec := s.post("/settings/prompt", url.Values{"name": {"summary"}, "text": {"Summarise {{.Title}}."}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := fileText(t, agents.PromptPath(dir, "summary")); got != "Summarise {{.Title}}.\n" {
		t.Errorf("prompts/summary.md: %q", got)
	}
	_, page := s.get("/settings")
	want(t, page, `<b>summary</b><span class="pill sm changed">Customised</span>`, `form="prr-summary"`)
	if rec := s.post("/settings/prompt/reset", url.Values{"name": {"summary"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("reset: %d", rec.Code)
	}
	if _, err := os.Stat(agents.PromptPath(dir, "summary")); !os.IsNotExist(err) {
		t.Error("prompts/summary.md is still there")
	}
}

// Every save needs the page's token, like every other action.
func TestSettingsNeedToken(t *testing.T) {
	s, dir := newSettingsSite(t)
	for _, path := range []string{"/settings", "/settings/reviewer", "/settings/reviewer/remove", "/settings/prompt", "/settings/prompt/reset"} {
		if rec := s.postWithoutToken(path, url.Values{"ORG": {"evil"}, "name": {"security"}}); rec.Code != http.StatusForbidden {
			t.Errorf("%s without the token: %d", path, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.env")); !os.IsNotExist(err) {
		t.Error("config.env was written")
	}
}

// ORG holds a list now, so the Account section marks every org in it, not the first and not the joined string.
// Matching ignores case, because GitHub's org: search does: ORG=ACME really does watch acme.
func TestSettingsAccountSeveralOrgs(t *testing.T) {
	s, _ := newSettingsSite(t)
	if err := config.Save(s.srv.cfg().ConfigDir, map[string]string{"ORG": "ACME, umbrella"}, nil); err != nil {
		t.Fatal(err)
	}
	code, page := s.get("/settings")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want(t, page, "acme · watched", "umbrella · watched")
	if strings.Contains(page, "isn't a member of") {
		t.Error("the account belongs to both orgs, but the page says it is not a member")
	}

	// One watched org the account is in, one it isn't: only the missing one is named.
	if err := config.Save(s.srv.cfg().ConfigDir, map[string]string{"ORG": "acme,initech"}, nil); err != nil {
		t.Fatal(err)
	}
	_, page = s.get("/settings")
	want(t, page, "acme · watched", "This account isn't a member of initech")
	if strings.Contains(page, "member of acme") {
		t.Error("acme is named as missing though the account belongs to it")
	}
}

// ORG is written in the form the page shows it back, so config.env and the field agree. Typed with spaces, saved
// with commas; a name repeated in another case is kept once, as the daemon reads it.
func TestSaveSettingsNormalisesORG(t *testing.T) {
	for _, c := range []struct{ typed, want string }{
		{"acme umbrella", "ORG=acme,umbrella\n"},
		{"acme,  umbrella ", "ORG=acme,umbrella\n"},
		{"acme,ACME", "ORG=acme\n"},
		{" acme ", "ORG=acme\n"},
	} {
		s, dir := newSettingsSite(t)
		if rec := s.post("/settings", url.Values{"ORG": {c.typed}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("ORG=%q: status %d", c.typed, rec.Code)
		}
		if env := fileText(t, filepath.Join(dir, "config.env")); !strings.Contains(env, c.want) {
			t.Errorf("ORG typed %q: config.env has no %q:\n%s", c.typed, c.want, env)
		}
	}
}
