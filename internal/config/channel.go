package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Channel is one slice of the bot's work: the repos it owns, reviewed with one Claude subscription, under one set of
// review settings, and surfaced one way. See docs/portable-channels-scope.md for the design.
//
// Today there is exactly one — the default channel, synthesized from config.env by DefaultChannel — so nothing yet
// behaves differently. Later phases load more channels from a checked-out file set and route events between them; this
// type is the seam they build on.
type Channel struct {
	Name    string         // stable identifier; "default" for the synthesized one
	Orgs    []string       // GitHub orgs this channel polls and owns (empty = anywhere), as the service-mode search uses
	Repos   RepoMatch      // optional finer ownership within those orgs (whitelist minus blacklist), for routing
	Claude  ClaudeAuth     // how its reviews authenticate to Claude (zero value = the ambient `claude` login)
	GitHub  GitHubAuth     // how it acts on GitHub (zero value = the ambient `gh` login)
	Review  ReviewSettings // its review knobs
	Surface Surface        // where its reviews are seen and announced
}

// RepoMatch narrows which repos a channel owns beyond its Orgs: a repo matches a Whitelist pattern and no Blacklist
// pattern, each a regular expression tested against "owner/name". An empty Whitelist means "every repo in Orgs". This
// holds the raw patterns; compiling them and deciding ownership across channels is a later phase (plan P1.2).
type RepoMatch struct {
	Whitelist []string
	Blacklist []string
}

// ClaudeAuth says how a channel's `claude` subprocess authenticates. The zero value means no override — the ambient
// login, which is today's behavior. Otherwise exactly one field is set and is resolved to subprocess environment in a
// later phase (P2): ConfigDir -> CLAUDE_CONFIG_DIR, OAuthTokenEnv -> CLAUDE_CODE_OAUTH_TOKEN, APIKeyEnv ->
// ANTHROPIC_API_KEY. The *Env fields name the environment variable that holds the secret, never the secret itself.
type ClaudeAuth struct {
	ConfigDir     string // a dedicated CLAUDE_CONFIG_DIR holding this channel's subscription login
	OAuthTokenEnv string // env var holding a `claude setup-token` OAuth token
	APIKeyEnv     string // env var holding an ANTHROPIC_API_KEY
}

// Env returns the environment overrides that select this channel's Claude account for a `claude` subprocess. It reads a
// secret from the environment variable the channel named, never storing it, and returns an error when that variable is
// unset so a misconfigured channel fails fast. The zero value returns nil — the ambient login (today's behavior). The
// caller appends these after the inherited environment, so they take precedence over any ambient credentials.
func (a ClaudeAuth) Env() ([]string, error) {
	switch {
	case a.ConfigDir != "":
		return []string{"CLAUDE_CONFIG_DIR=" + a.ConfigDir}, nil
	case a.OAuthTokenEnv != "":
		v, ok := os.LookupEnv(a.OAuthTokenEnv)
		if !ok {
			return nil, fmt.Errorf("the OAuth-token env var %q is not set", a.OAuthTokenEnv)
		}
		return []string{"CLAUDE_CODE_OAUTH_TOKEN=" + v}, nil
	case a.APIKeyEnv != "":
		v, ok := os.LookupEnv(a.APIKeyEnv)
		if !ok {
			return nil, fmt.Errorf("the API-key env var %q is not set", a.APIKeyEnv)
		}
		return []string{"ANTHROPIC_API_KEY=" + v}, nil
	}
	return nil, nil
}

// GitHubAuth says how a channel acts on GitHub. The zero value means the ambient `gh` login (today's behavior).
// TokenEnv names an env var holding a token; App names a GitHub App installation, wired in a later phase (P7). As with
// ClaudeAuth, a secret is referenced by env-var name, never stored here.
type GitHubAuth struct {
	TokenEnv string // env var holding a GitHub token
	App      string // a GitHub App installation reference, resolved later
}

// Token resolves this channel's GitHub token from the environment variable it named, never storing the secret. It
// returns "" when the channel configures no token (the ambient gh login, today's behavior), and an error when the
// named variable is unset so a misconfigured channel fails fast — mirroring ClaudeAuth.Env.
func (a GitHubAuth) Token() (string, error) {
	if a.TokenEnv == "" {
		return "", nil
	}
	v, ok := os.LookupEnv(a.TokenEnv)
	if !ok {
		return "", fmt.Errorf("the GitHub-token env var %q is not set", a.TokenEnv)
	}
	return v, nil
}

// ReviewSettings are the per-channel review knobs. The default channel copies them from the global config; it is kept
// small on purpose and extended as later phases make more settings per-channel.
type ReviewSettings struct {
	Threshold int      // verified certainty (0-100) at which a confirmed finding is posted without asking
	AutoPost  []string // severities the bot may post on its own (empty = none)
}

// Surface is where a channel's reviews are seen and announced. The zero value is the built-in web UI with no outbound
// notifier — today's behavior. Notify names an outbound target, resolved in a later phase (P8).
type Surface struct {
	Notify string // an outbound notifier target reference; empty = web UI only
}

// DefaultChannel is the single channel the bot runs when no channel config is present: today's settings, expressed as
// one channel named "default", using the ambient Claude and GitHub logins and the web UI. It lets the channel seam be
// introduced without changing behavior — the review and poll paths can read a Channel while there is still only ever
// this one, whose Orgs and Review come straight from the global config.
func DefaultChannel(c Config) Channel {
	return Channel{
		Name:   "default",
		Orgs:   c.Orgs,
		Review: ReviewSettings{Threshold: c.Threshold, AutoPost: c.AutoPost},
		// Repos, Claude, GitHub and Surface are left zero: owns every repo in Orgs, the ambient logins, the web UI.
	}
}

// channelsSubdir is where per-channel config files live under the config directory. Each "<name>.env" there is one
// channel, in the same KEY=value format as config.env, and the file name (without .env) is the channel's name.
const channelsSubdir = "channels"

// LoadChannels reads the channels for config directory dir, given the global config c (what config.env parsed to). Each
// <name>.env under dir/channels is one channel; a key it omits is inherited from c, so a channel states only what
// differs. When there is no channels directory, or it holds no .env files, the bot runs the single default channel —
// today's behavior. Channels come back sorted by name. Unlike the global config, a channel file's values are read from
// the file alone (no environment override): the environment configures the process, the files configure the channels.
func LoadChannels(dir string, c Config) ([]Channel, error) {
	entries, err := os.ReadDir(filepath.Join(dir, channelsSubdir))
	if err != nil {
		if os.IsNotExist(err) {
			return []Channel{DefaultChannel(c)}, nil
		}
		return nil, err
	}
	var chs []Channel
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".env")
		chs = append(chs, channelFrom(name, readEnvFile(filepath.Join(dir, channelsSubdir, e.Name())), c))
	}
	if len(chs) == 0 {
		return []Channel{DefaultChannel(c)}, nil
	}
	slices.SortFunc(chs, func(a, b Channel) int { return strings.Compare(a.Name, b.Name) })
	return chs, nil
}

// channelFrom builds one channel from a parsed <name>.env file, inheriting from the global config c anything the file
// omits. Values are read straight from the file map m (not through settings.lookup), so a process-wide env var can't
// leak into a channel. Auth and surface default to their zero values (the ambient logins, the web UI).
func channelFrom(name string, m settings, c Config) Channel {
	ch := Channel{
		Name:    name,
		Orgs:    c.Orgs,
		Repos:   RepoMatch{Whitelist: list(m["REPOS_ALLOW"]), Blacklist: list(m["REPOS_DENY"])},
		Claude:  ClaudeAuth{ConfigDir: m["CLAUDE_CONFIG_DIR"], OAuthTokenEnv: m["CLAUDE_OAUTH_TOKEN_ENV"], APIKeyEnv: m["ANTHROPIC_API_KEY_ENV"]},
		GitHub:  GitHubAuth{TokenEnv: m["GITHUB_TOKEN_ENV"], App: m["GITHUB_APP"]},
		Review:  ReviewSettings{Threshold: c.Threshold, AutoPost: c.AutoPost},
		Surface: Surface{Notify: m["NOTIFY"]},
	}
	if v, ok := m["ORG"]; ok {
		ch.Orgs = SplitOrgs(v)
	}
	if v, ok := m["THRESHOLD"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			ch.Review.Threshold = n
		}
	}
	if v, ok := m["AUTO_POST"]; ok {
		ch.Review.AutoPost = severitiesOf(list(v))
	}
	return ch
}

// severitiesOf keeps only the known severities from a list, de-duplicated, so a stray or misspelt word in a channel's
// AUTO_POST can only tighten the rule, never widen it — the same guarantee autoPost gives the global config.
func severitiesOf(xs []string) []string {
	var out []string
	for _, x := range xs {
		x = strings.ToLower(strings.TrimSpace(x))
		if slices.Contains(severities, x) && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
