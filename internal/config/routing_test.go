package config

import (
	"strings"
	"testing"
)

// A specific channel is tried before the catch-all; the owner is matched case-insensitively; whitelist narrows and
// blacklist excludes; and a repo no specific channel owns falls to the catch-all.
func TestRouterChannelFor(t *testing.T) {
	company := Channel{Name: "company", Orgs: []string{"acme", "acme-labs"}, Repos: RepoMatch{Blacklist: []string{"^acme/archived-"}}}
	personal := Channel{Name: "personal", Orgs: []string{"me"}}
	oss := Channel{Name: "oss", Repos: RepoMatch{Whitelist: []string{"^contrib/"}}} // pattern-only, any owner
	fallback := Channel{Name: "fallback"}                                           // catch-all

	r, err := NewRouter([]Channel{company, personal, oss, fallback})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		repo string
		want string
	}{
		{"acme/web", "company"},
		{"ACME/web", "company"},            // owner case-insensitive
		{"acme/archived-2019", "fallback"}, // blacklisted on company, so it falls through
		{"me/dotfiles", "personal"},
		{"contrib/tool", "oss"},        // pattern-only channel, any owner
		{"stranger/thing", "fallback"}, // nothing specific owns it
	}
	for _, c := range cases {
		got, ok := r.ChannelFor(c.repo)
		if !ok || got.Name != c.want {
			t.Errorf("ChannelFor(%q) = %q (ok=%v), want %q", c.repo, got.Name, ok, c.want)
		}
	}
}

// Without a catch-all, a repo no specific channel owns is unrouted.
func TestRouterNoFallback(t *testing.T) {
	r, err := NewRouter([]Channel{{Name: "company", Orgs: []string{"acme"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ChannelFor("acme/web"); !ok {
		t.Error("acme/web should route to company")
	}
	if ch, ok := r.ChannelFor("other/repo"); ok {
		t.Errorf("other/repo should be unrouted, got %q", ch.Name)
	}
}

// NewRouter rejects configs that can't route cleanly.
func TestNewRouterErrors(t *testing.T) {
	cases := []struct {
		name string
		chs  []Channel
		want string
	}{
		{"invalid regex", []Channel{{Name: "x", Repos: RepoMatch{Whitelist: []string{"("}}}}, "pattern"},
		{"duplicate names", []Channel{{Name: "dup"}, {Name: "dup"}}, "named"},
		{"two catch-alls", []Channel{{Name: "a"}, {Name: "b"}}, "catch-all"},
		{"shared org", []Channel{{Name: "a", Orgs: []string{"acme"}}, {Name: "b", Orgs: []string{"ACME", "other"}}}, "both own org"},
	}
	for _, c := range cases {
		if _, err := NewRouter(c.chs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}
