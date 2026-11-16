// Package webhook receives GitHub pull_request webhooks: it verifies the delivery signature, parses the event, and
// hands a reviewable one to a callback. It is deliberately free of the review machinery — the caller supplies what to
// do with an event — so the HTTP contract (signature, parsing, status codes) is small and fully testable.
//
// Not exercised here (it needs GitHub actually delivering): a real signed delivery end to end. The signature check,
// the parsing and the dispatch are all tested with synthesized requests.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxBody caps a webhook body. GitHub's deliveries are well under this; the cap stops a hostile sender from streaming
// an unbounded body into memory before the signature is even checked.
const maxBody = 5 << 20

// Event is the reviewable part of a pull_request webhook.
type Event struct {
	Repo           string // owner/name
	Number         int
	Action         string
	HeadSHA        string
	InstallationID int64 // the GitHub App installation that delivered it (0 when not an App delivery)
}

// reviewActions are the pull_request actions worth a review: a new PR, a push to one, a reopen, or a draft marked
// ready. Other actions (labeled, assigned, closed, …) are acknowledged and ignored.
var reviewActions = map[string]bool{
	"opened":           true,
	"synchronize":      true,
	"reopened":         true,
	"ready_for_review": true,
}

// ValidSignature reports whether sig — the X-Hub-Signature-256 header — is GitHub's HMAC-SHA256 of body under secret.
// The compare is constant-time; an empty secret or a header not of the form "sha256=<hex>" is never valid.
func ValidSignature(secret, sig string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(want))
}

// Parse decodes a pull_request webhook and reports whether it is worth reviewing. It returns ok=false for any other
// event type, for an action not in reviewActions, for a still-draft PR (unless it was just marked ready), and for a
// payload missing the repo or PR number.
func Parse(eventType string, body []byte) (Event, bool) {
	if eventType != "pull_request" {
		return Event{}, false
	}
	var p struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			Number int  `json:"number"`
			Draft  bool `json:"draft"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if json.Unmarshal(body, &p) != nil {
		return Event{}, false
	}
	if !reviewActions[p.Action] {
		return Event{}, false
	}
	if p.PullRequest.Draft && p.Action != "ready_for_review" {
		return Event{}, false
	}
	num := p.Number
	if num == 0 {
		num = p.PullRequest.Number
	}
	if p.Repository.FullName == "" || num == 0 {
		return Event{}, false
	}
	return Event{
		Repo:           p.Repository.FullName,
		Number:         num,
		Action:         p.Action,
		HeadSHA:        p.PullRequest.Head.SHA,
		InstallationID: p.Installation.ID,
	}, true
}

// Handler is an http.Handler for GitHub's webhook deliveries. It verifies the signature, parses the event, and for a
// reviewable pull_request event starts Review in its own goroutine, answering 202 at once so a review (which takes
// minutes) never holds GitHub's delivery open. A bad signature is 401; an event with nothing to do is 204.
type Handler struct {
	Secret string
	Review func(ctx context.Context, ev Event) // started in a new goroutine; must not block on the HTTP response
	Log    func(format string, args ...any)
	// Base is the context the detached review runs under (the server's lifetime). nil means context.Background.
	Base context.Context
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !ValidSignature(h.Secret, r.Header.Get("X-Hub-Signature-256"), body) {
		h.logf("rejected a delivery with a bad signature")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	ev, ok := Parse(r.Header.Get("X-GitHub-Event"), body)
	if !ok {
		w.WriteHeader(http.StatusNoContent) // acknowledged; nothing to review
		return
	}
	base := h.Base
	if base == nil {
		base = context.Background()
	}
	go h.Review(base, ev)
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}
