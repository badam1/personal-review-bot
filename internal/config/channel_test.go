package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The default channel represents today's settings as one channel, using the ambient Claude and GitHub logins — so the
// channel seam can be introduced without changing behavior.
func TestDefaultChannel(t *testing.T) {
	c := Config{Orgs: []string{"acme", "acme-labs"}, Threshold: 85, AutoPost: []string{"low", "medium"}}
	ch := DefaultChannel(c)

	if ch.Name != "default" {
		t.Errorf("name = %q, want default", ch.Name)
	}
	if !slices.Equal(ch.Orgs, c.Orgs) {
		t.Errorf("orgs = %v, want the global orgs %v", ch.Orgs, c.Orgs)
	}
	if len(ch.Repos.Whitelist) != 0 || len(ch.Repos.Blacklist) != 0 {
		t.Errorf("repo match = %+v, want empty (owns every repo in its orgs)", ch.Repos)
	}
	if ch.Review.Threshold != 85 || !slices.Equal(ch.Review.AutoPost, []string{"low", "medium"}) {
		t.Errorf("review = %+v, want the global threshold and auto-post", ch.Review)
	}
	// Ambient logins and web-only surface: the zero values, i.e. today's behavior.
	if (ch.Claude != ClaudeAuth{}) {
		t.Errorf("claude = %+v, want the ambient login (zero value)", ch.Claude)
	}
	if (ch.GitHub != GitHubAuth{}) {
		t.Errorf("github = %+v, want the ambient gh login (zero value)", ch.GitHub)
	}
	if (ch.Surface != Surface{}) {
		t.Errorf("surface = %+v, want web UI only (zero value)", ch.Surface)
	}
}

// With no ORG set, the default channel owns any repo anywhere — empty orgs, matching today's "review anywhere".
func TestDefaultChannelNoOrgs(t *testing.T) {
	if orgs := DefaultChannel(Config{}).Orgs; len(orgs) != 0 {
		t.Errorf("orgs = %v, want empty (anywhere) when no orgs are set", orgs)
	}
}

// No channels/ directory means the bot runs the single default channel — today's behavior.
func TestLoadChannelsNone(t *testing.T) {
	c := Config{Orgs: []string{"acme"}, Threshold: 80, AutoPost: []string{"low", "medium"}}
	chs, err := LoadChannels(t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 1 || chs[0].Name != "default" {
		t.Fatalf("want a single default channel, got %+v", chs)
	}
}

// Each <name>.env under channels/ is one channel; it inherits anything it omits from the global config, non-.env files
// are ignored, and the channels come back sorted by name.
func TestLoadChannels(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "channels")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(cdir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("company.env", "ORG=acme, acme-labs\nCLAUDE_CONFIG_DIR=/claude/company\nTHRESHOLD=90\nREPOS_DENY=^acme/archived-\n")
	write("personal.env", "ORG=me\nAUTO_POST=low\nCLAUDE_OAUTH_TOKEN_ENV=PRB_PERSONAL_TOKEN\nGITHUB_TOKEN_ENV=PRB_PERSONAL_GH\nNOTIFY=slack-webhook-env\n")
	write("inherit.env", "# only auth here; everything else comes from the global config\nCLAUDE_CONFIG_DIR=/claude/inh\n")
	write("notes.txt", "ignored: not a .env file\n")

	c := Config{Orgs: []string{"global"}, Threshold: 80, AutoPost: []string{"low", "medium"}}
	chs, err := LoadChannels(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]Channel{}
	for _, ch := range chs {
		names = append(names, ch.Name)
		byName[ch.Name] = ch
	}
	if want := []string{"company", "inherit", "personal"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v (sorted; non-.env ignored)", names, want)
	}

	co := byName["company"]
	if !slices.Equal(co.Orgs, []string{"acme", "acme-labs"}) {
		t.Errorf("company orgs = %v", co.Orgs)
	}
	if co.Claude.ConfigDir != "/claude/company" {
		t.Errorf("company claude = %+v", co.Claude)
	}
	if co.Review.Threshold != 90 {
		t.Errorf("company threshold = %d, want 90 (overridden)", co.Review.Threshold)
	}
	if !slices.Equal(co.Review.AutoPost, []string{"low", "medium"}) {
		t.Errorf("company auto-post = %v, want the inherited [low medium]", co.Review.AutoPost)
	}
	if !slices.Equal(co.Repos.Blacklist, []string{"^acme/archived-"}) {
		t.Errorf("company blacklist = %v", co.Repos.Blacklist)
	}

	pe := byName["personal"]
	if !slices.Equal(pe.Orgs, []string{"me"}) {
		t.Errorf("personal orgs = %v", pe.Orgs)
	}
	if pe.Review.Threshold != 80 {
		t.Errorf("personal threshold = %d, want the inherited 80", pe.Review.Threshold)
	}
	if !slices.Equal(pe.Review.AutoPost, []string{"low"}) {
		t.Errorf("personal auto-post = %v, want [low] (overridden)", pe.Review.AutoPost)
	}
	if pe.Claude.OAuthTokenEnv != "PRB_PERSONAL_TOKEN" {
		t.Errorf("personal claude = %+v", pe.Claude)
	}
	if pe.GitHub.TokenEnv != "PRB_PERSONAL_GH" {
		t.Errorf("personal github = %+v", pe.GitHub)
	}
	if pe.Surface.Notify != "slack-webhook-env" {
		t.Errorf("personal surface = %+v", pe.Surface)
	}

	inh := byName["inherit"]
	if !slices.Equal(inh.Orgs, []string{"global"}) || inh.Review.Threshold != 80 ||
		!slices.Equal(inh.Review.AutoPost, []string{"low", "medium"}) {
		t.Errorf("inherit channel should take orgs/review from the global config, got orgs=%v review=%+v", inh.Orgs, inh.Review)
	}
	if inh.Claude.ConfigDir != "/claude/inh" {
		t.Errorf("inherit claude = %+v", inh.Claude)
	}
}
