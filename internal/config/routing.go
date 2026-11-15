package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Router decides which channel owns a repo. It compiles each channel's whitelist/blacklist patterns once and tries a
// specific channel (one that names orgs or patterns) before the catch-all, so a repo resolves to exactly one channel.
// Build it with NewRouter, which rejects a config that can't route cleanly: an invalid pattern, a duplicate channel
// name, two catch-alls, or two channels that both claim an org without narrowing.
type Router struct {
	specific []compiledChannel // channels that name orgs or patterns, in load order
	fallback *compiledChannel  // the one catch-all (no orgs, no whitelist), if any
}

type compiledChannel struct {
	ch        Channel
	whitelist []*regexp.Regexp
	blacklist []*regexp.Regexp
}

// NewRouter compiles and validates the channels, returning a Router or the first configuration error.
func NewRouter(chs []Channel) (*Router, error) {
	var r Router
	seen := map[string]bool{}
	for _, ch := range chs {
		if seen[ch.Name] {
			return nil, fmt.Errorf("two channels are named %q", ch.Name)
		}
		seen[ch.Name] = true
		cc, err := compileChannel(ch)
		if err != nil {
			return nil, err
		}
		if isFallback(ch) {
			if r.fallback != nil {
				return nil, fmt.Errorf("channels %q and %q are both catch-alls (no orgs, no REPOS_ALLOW); keep one", r.fallback.ch.Name, ch.Name)
			}
			f := cc
			r.fallback = &f
			continue
		}
		r.specific = append(r.specific, cc)
	}
	if err := noSharedOrg(r.specific); err != nil {
		return nil, err
	}
	return &r, nil
}

// ChannelFor returns the channel that owns repo ("owner/name"), trying the specific channels in order and then the
// catch-all. It reports false when nothing owns it.
func (r *Router) ChannelFor(repo string) (Channel, bool) {
	for _, cc := range r.specific {
		if cc.owns(repo) {
			return cc.ch, true
		}
	}
	if r.fallback != nil && r.fallback.owns(repo) {
		return r.fallback.ch, true
	}
	return Channel{}, false
}

func compileChannel(ch Channel) (compiledChannel, error) {
	cc := compiledChannel{ch: ch}
	for _, g := range []struct {
		label string
		pats  []string
		dst   *[]*regexp.Regexp
	}{
		{"REPOS_ALLOW", ch.Repos.Whitelist, &cc.whitelist},
		{"REPOS_DENY", ch.Repos.Blacklist, &cc.blacklist},
	} {
		for _, p := range g.pats {
			re, err := regexp.Compile(p)
			if err != nil {
				return compiledChannel{}, fmt.Errorf("channel %q: %s pattern %q: %w", ch.Name, g.label, p, err)
			}
			*g.dst = append(*g.dst, re)
		}
	}
	return cc, nil
}

// owns reports whether this channel owns repo. The owner must be in the channel's orgs (an empty org list means any
// owner); the repo must match the whitelist if it has one; and it must match no blacklist pattern. Patterns are tested
// against the full "owner/name".
func (cc compiledChannel) owns(repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	if len(cc.ch.Orgs) > 0 && !containsFold(cc.ch.Orgs, owner) {
		return false
	}
	if len(cc.whitelist) > 0 && !anyMatch(cc.whitelist, repo) {
		return false
	}
	return !anyMatch(cc.blacklist, repo)
}

// isFallback reports whether a channel is the catch-all: it names no orgs and no whitelist, so it would own any repo.
func isFallback(ch Channel) bool { return len(ch.Orgs) == 0 && len(ch.Repos.Whitelist) == 0 }

// noSharedOrg rejects two channels that both claim repos in the same org without narrowing — first-match would then
// route by load order, which is far more likely a mistake than intent. A channel with a REPOS_ALLOW is taken to narrow
// itself and is exempt.
func noSharedOrg(ccs []compiledChannel) error {
	for i := range ccs {
		for j := i + 1; j < len(ccs); j++ {
			a, b := ccs[i].ch, ccs[j].ch
			if len(a.Repos.Whitelist) > 0 || len(b.Repos.Whitelist) > 0 {
				continue
			}
			for _, o := range a.Orgs {
				if containsFold(b.Orgs, o) {
					return fmt.Errorf("channels %q and %q both own org %q; narrow one with REPOS_ALLOW/REPOS_DENY or split the orgs", a.Name, b.Name, o)
				}
			}
		}
	}
	return nil
}

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func containsFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
