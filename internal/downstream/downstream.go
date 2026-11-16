// Package downstream reduces what happened to a posted finding — resolved, argued with, reacted to — to one signal.
// It is a pure reduction with no dependencies, so the store, the GitHub client and the daemon can all depend on it:
// the daemon matches a finding to the bot's own review thread (via pipeline.MatchThread) and reads the thread's state;
// this package turns that state into the one word the Stats page shows.
package downstream

// State is what was observed about one posted comment's thread at a point in time.
type State struct {
	Exists     bool // the bot's comment is still on the PR (false = it was deleted)
	Resolved   bool // its review thread is marked resolved
	ThumbsUp   int  // 👍 reactions on the comment
	ThumbsDown int  // 👎 reactions on the comment
	Replies    int  // replies in the thread, by anyone (context, not part of the verdict — a reply can agree or argue)
}

// Signal is the one-word verdict the Stats page reads. It is deliberately coarse: these are the distinctions we can
// draw reliably from a public API without reading tone.
type Signal string

const (
	Unknown  Signal = "unknown"  // not checked yet, or the comment couldn't be matched to the bot's thread
	Quiet    Signal = "quiet"    // still there, nothing happened — the resting state of a fair post
	Endorsed Signal = "endorsed" // thumbed up, or its thread resolved, with no thumbs-down
	Disputed Signal = "disputed" // thumbed down
	Removed  Signal = "removed"  // the comment was deleted off the PR (reserved: emitted only with a reliable deletion read)
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
