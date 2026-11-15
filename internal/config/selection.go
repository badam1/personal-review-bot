package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// SelectChannels returns the channels an instance should run, given a selection spec (the --channels flag, or the
// CHANNELS environment variable). An empty spec selects all of them.
//
// Otherwise the spec is a comma- or space-separated list of name patterns — regular expressions, full-matched against
// channel names — and a pattern prefixed with "!" excludes. A channel is selected when it matches at least one include
// (or there are no includes) and no exclude. An include that matches no channel is an error, as is a spec that selects
// nothing, so a typo fails fast at startup rather than silently running the wrong set.
func SelectChannels(chs []Channel, spec string) ([]Channel, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return chs, nil
	}
	var includes, excludes []*regexp.Regexp
	var includeSrc []string
	for _, tok := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		pat, exclude := strings.TrimPrefix(tok, "!"), strings.HasPrefix(tok, "!")
		re, err := regexp.Compile("^(?:" + pat + ")$")
		if err != nil {
			return nil, fmt.Errorf("--channels pattern %q: %w", tok, err)
		}
		if exclude {
			excludes = append(excludes, re)
		} else {
			includes = append(includes, re)
			includeSrc = append(includeSrc, pat)
		}
	}
	for i, re := range includes {
		if !slices.ContainsFunc(chs, func(ch Channel) bool { return re.MatchString(ch.Name) }) {
			return nil, fmt.Errorf("--channels: %q matches no channel", includeSrc[i])
		}
	}
	var out []Channel
	for _, ch := range chs {
		if len(includes) > 0 && !anyMatch(includes, ch.Name) {
			continue
		}
		if anyMatch(excludes, ch.Name) {
			continue
		}
		out = append(out, ch)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--channels %q selects no channel", spec)
	}
	return out, nil
}
