package agents

import (
	"context"
	"os"
	"testing"
	"time"
)

// Run against the real claude CLI (about $0.01 of Haiku) with: PERSONAL_REVIEW_BOT_LIVE_CLAUDE=1 go test ./internal/agents -run Live -v
func TestClaudeLive(t *testing.T) {
	if os.Getenv("PERSONAL_REVIEW_BOT_LIVE_CLAUDE") == "" {
		t.Skip("set PERSONAL_REVIEW_BOT_LIVE_CLAUDE=1 to run")
	}
	u, err := CheckUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	five, week := u.At(time.Now())
	t.Logf("usage: 5h %.0f%% (resets %s), week %.0f%% (resets %s)", five*100, u.FiveHourResets.Local().Format(time.Kitchen),
		week*100, u.WeekResets.Local().Format("Mon 15:04"))

	// The reviewers' path: schema, read-only tools, an effort and the stream parsed back into a result.
	schema := `{"type":"object","required":["files"],"properties":{"files":{"type":"integer"}}}`
	raw, r, err := run(context.Background(), ".", "haiku", "low", "How many .go files are in this directory? Use Glob.",
		schema, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil || r.Turns == 0 || r.Cost == 0 {
		t.Errorf("run lost its stats or usage: %+v", r)
	}
	t.Logf("result %s · %d turns · $%.4f", raw, r.Turns, r.Cost)
}
