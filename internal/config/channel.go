package config

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

// GitHubAuth says how a channel acts on GitHub. The zero value means the ambient `gh` login (today's behavior).
// TokenEnv names an env var holding a token; App names a GitHub App installation, wired in a later phase (P7). As with
// ClaudeAuth, a secret is referenced by env-var name, never stored here.
type GitHubAuth struct {
	TokenEnv string // env var holding a GitHub token
	App      string // a GitHub App installation reference, resolved later
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
