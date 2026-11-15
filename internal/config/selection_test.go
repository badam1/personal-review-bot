package config

import (
	"strings"
	"testing"
)

func TestSelectChannels(t *testing.T) {
	chs := []Channel{{Name: "company"}, {Name: "personal"}, {Name: "team-web"}, {Name: "team-api"}}
	names := func(got []Channel) []string {
		var ns []string
		for _, c := range got {
			ns = append(ns, c.Name)
		}
		return ns
	}
	cases := []struct {
		spec string
		want []string
	}{
		{"", []string{"company", "personal", "team-web", "team-api"}}, // empty = all
		{"company", []string{"company"}},                              // exact name
		{"company, personal", []string{"company", "personal"}},        // list
		{"team-.*", []string{"team-web", "team-api"}},                 // pattern
		{"!personal", []string{"company", "team-web", "team-api"}},    // exclude only
		{"team-.*, !team-api", []string{"team-web"}},                  // include then exclude
	}
	for _, c := range cases {
		got, err := SelectChannels(chs, c.spec)
		if err != nil {
			t.Errorf("SelectChannels(%q): %v", c.spec, err)
			continue
		}
		if g := names(got); !equalUnordered(g, c.want) {
			t.Errorf("SelectChannels(%q) = %v, want %v", c.spec, g, c.want)
		}
	}
}

func TestSelectChannelsErrors(t *testing.T) {
	chs := []Channel{{Name: "company"}, {Name: "personal"}}
	cases := []struct {
		spec string
		want string
	}{
		{"nope", "matches no channel"}, // typo fails fast
		{"!company, !personal", "selects no channel"},
		{"(", "pattern"}, // invalid regex
	}
	for _, c := range cases {
		if _, err := SelectChannels(chs, c.spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("SelectChannels(%q) err = %v, want one containing %q", c.spec, err, c.want)
		}
	}
}

func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
