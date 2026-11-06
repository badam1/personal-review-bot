// Package downstream reads what happened to a finding after the bot posted it — resolved, argued with, reacted to,
// or removed — and reduces that to one signal. The reading is behind an interface so the poller runs in tests without
// GitHub; the reduction is a pure function so the rule can change without touching the fetch. Nothing here imports the
// store or the GitHub client, so both can depend on it.
package downstream

import (
	"context"
	"time"
)

// State is what was observed about one posted comment's thread at a point in time.
type State struct {
	Exists     bool // the comment is still on the PR (false = it was deleted)
	Resolved   bool // its review thread is marked resolved
	ThumbsUp   int  // 👍 reactions on the comment
	ThumbsDown int  // 👎 reactions on the comment
	Replies    int  // replies in the thread, by anyone (context, not part of the verdict — a reply can agree or argue)
}

// Signal is the one-word verdict the dashboard reads. It is deliberately coarse: these are the distinctions we can draw
// reliably from a public API without reading tone.
type Signal string

const (
	Unknown  Signal = "unknown"  // not checked yet, or the comment couldn't be matched to a thread
	Quiet    Signal = "quiet"    // still there, nothing happened — the resting state of a fair post
	Endorsed Signal = "endorsed" // thumbed up, or its thread resolved, with no thumbs-down
	Disputed Signal = "disputed" // thumbed down
	Removed  Signal = "removed"  // the comment was deleted off the PR
)

// Interpret reduces an observed State to a Signal. Pure, and the order is the point: a deleted comment is Removed
// whatever else was on it; a thumbs-down outranks a thumbs-up or a resolve, because a reader went out of their way to
// mark it wrong. Everything else that drew no reaction is Quiet, not a failure.
func Interpret(s State) Signal {
	switch {
	case !s.Exists:
		return Removed
	case s.ThumbsDown > 0:
		return Disputed
	case s.ThumbsUp > 0 || s.Resolved:
		return Endorsed
	default:
		return Quiet
	}
}

// Thread is one review thread on a PR: where it sits, and the state of its first (the bot's) comment. The poller
// matches a finding to a thread by Path and Line.
type Thread struct {
	Path     string
	Line     int // the line as it is now (0 when the thread is outdated — the line has since changed)
	OrigLine int // the line it was started on; what the bot's comment was posted against, so what we stored matches it
	State    State
}

// Checker reads the review threads of one pull request. It is the single seam onto GitHub: the poller takes a Checker,
// so tests pass a fake and nothing reaches the network.
type Checker interface {
	Threads(ctx context.Context, repo string, number int) ([]Thread, error)
}

// Match finds the thread a posted finding belongs to, by file and line. A finding with no line (a design note) or no
// matching thread returns false, and the poller leaves such a finding Unknown rather than guessing.
func Match(path string, line int, threads []Thread) (Thread, bool) {
	if line <= 0 {
		return Thread{}, false
	}
	for _, t := range threads {
		if t.Path == path && (t.Line == line || t.OrigLine == line) {
			return t, true
		}
	}
	return Thread{}, false
}

// Now is the clock the poller stamps checks with; a test can replace it.
var Now = func() time.Time { return time.Now().UTC() }

// Posted is what the poller needs about a finding the bot put on GitHub: which comment to find, and where.
type Posted struct {
	ID     int64
	Repo   string // owner/name
	Number int
	Path   string
	Line   int
}

// Result is one finding's checked signal, for the caller to persist.
type Result struct {
	ID     int64
	Signal Signal
	At     time.Time
}

// Poll checks each posted finding's downstream state once and returns the results for the caller to store. It fetches
// each PR's threads once (findings are grouped by PR), and a Checker error on one PR is logged and skipped so one
// unreachable PR can't stop the rest. A finding whose thread can't be located stays Unknown rather than a guess.
func Poll(ctx context.Context, posted []Posted, chk Checker, logf func(string, ...any)) []Result {
	type key struct {
		repo   string
		number int
	}
	groups := map[key][]Posted{}
	var order []key
	for _, p := range posted {
		k := key{p.Repo, p.Number}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	var out []Result
	for _, k := range order {
		threads, err := chk.Threads(ctx, k.repo, k.number)
		if err != nil {
			if logf != nil {
				logf("downstream %s#%d: %v", k.repo, k.number, err)
			}
			continue
		}
		at := Now()
		for _, p := range groups[k] {
			sig := Unknown
			if t, ok := Match(p.Path, p.Line, threads); ok {
				sig = Interpret(t.State)
			}
			out = append(out, Result{ID: p.ID, Signal: sig, At: at})
		}
	}
	return out
}
