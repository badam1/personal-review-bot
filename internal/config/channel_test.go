package config

import (
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
