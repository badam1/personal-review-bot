package daemon

import (
	"context"
	"testing"

	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/webhook"
)

// With no ambient login and no channel client, a webhook review is skipped before any GitHub call — it must not try
// (and nil-panic) to review under an identity it does not have.
func TestReviewOnceSkipsWithoutIdentity(t *testing.T) {
	base := pipeline.Deps{Log: func(string, ...any) {}} // GH nil, Clients nil
	ev := webhook.Event{Repo: "o/r", Number: 1, HeadSHA: "abc", Action: "opened"}
	reviewOnce(context.Background(), base, ev, "nochannel") // returns without panicking or reaching GitHub
}

func TestChannelLabel(t *testing.T) {
	if got := channelLabel(""); got != "default" {
		t.Errorf("channelLabel(\"\") = %q, want default", got)
	}
	if got := channelLabel("company"); got != "company" {
		t.Errorf("channelLabel(company) = %q, want company", got)
	}
}
