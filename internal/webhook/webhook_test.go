package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ValidSignature accepts only GitHub's HMAC-SHA256 of the exact body under the exact secret.
func TestValidSignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	good := sign("s3cr3t", body)
	if !ValidSignature("s3cr3t", good, body) {
		t.Error("the correct signature should verify")
	}
	if ValidSignature("wrong", good, body) {
		t.Error("a different secret must not verify")
	}
	if ValidSignature("s3cr3t", good, append(body, '!')) {
		t.Error("a tampered body must not verify")
	}
	if ValidSignature("", good, body) {
		t.Error("an empty secret is never valid")
	}
	if ValidSignature("s3cr3t", "deadbeef", body) {
		t.Error("a header without the sha256= prefix is never valid")
	}
}

// Parse keeps the reviewable pull_request events and drops the rest.
func TestParse(t *testing.T) {
	pr := func(action string, draft bool) []byte {
		d := "false"
		if draft {
			d = "true"
		}
		return []byte(`{"action":"` + action + `","number":7,"pull_request":{"number":7,"draft":` + d +
			`,"head":{"sha":"abc123"}},"repository":{"full_name":"acme/web"},"installation":{"id":42}}`)
	}

	ev, ok := Parse("pull_request", pr("opened", false))
	if !ok {
		t.Fatal("opened should be reviewable")
	}
	if ev.Repo != "acme/web" || ev.Number != 7 || ev.Action != "opened" || ev.HeadSHA != "abc123" || ev.InstallationID != 42 {
		t.Errorf("event = %+v", ev)
	}

	for _, a := range []string{"synchronize", "reopened", "ready_for_review"} {
		if _, ok := Parse("pull_request", pr(a, false)); !ok {
			t.Errorf("%s should be reviewable", a)
		}
	}
	for _, a := range []string{"labeled", "assigned", "closed", "edited"} {
		if _, ok := Parse("pull_request", pr(a, false)); ok {
			t.Errorf("%s should be ignored", a)
		}
	}
	// A still-draft PR is skipped, but the moment it is marked ready it is reviewed.
	if _, ok := Parse("pull_request", pr("opened", true)); ok {
		t.Error("a draft opened PR should be skipped")
	}
	if _, ok := Parse("pull_request", pr("ready_for_review", true)); !ok {
		t.Error("a PR marked ready should be reviewed even though draft is still true in the payload")
	}
	// Wrong event type, and a malformed / incomplete payload.
	if _, ok := Parse("push", pr("opened", false)); ok {
		t.Error("a non-pull_request event should be ignored")
	}
	if _, ok := Parse("pull_request", []byte("not json")); ok {
		t.Error("a malformed payload should be ignored")
	}
	if _, ok := Parse("pull_request", []byte(`{"action":"opened","pull_request":{"head":{"sha":"x"}}}`)); ok {
		t.Error("a payload with no repo or number should be ignored")
	}
}

// The handler verifies, parses, and dispatches: a signed reviewable event is 202 and reaches Review; a bad signature
// is 401 and never does; an event with nothing to do is 204.
func TestHandler(t *testing.T) {
	const secret = "hooksecret"
	body := []byte(`{"action":"opened","number":7,"pull_request":{"number":7,"draft":false,"head":{"sha":"abc123"}},` +
		`"repository":{"full_name":"acme/web"},"installation":{"id":42}}`)

	post := func(h *Handler, sig, event string, b []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(b)))
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		req.Header.Set("X-GitHub-Event", event)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Signed, reviewable: 202 and Review is called with the parsed event.
	done := make(chan Event, 1)
	h := &Handler{Secret: secret, Log: t.Logf, Review: func(_ context.Context, ev Event) { done <- ev }}
	if rr := post(h, sign(secret, body), "pull_request", body); rr.Code != http.StatusAccepted {
		t.Errorf("signed reviewable: status %d, want 202", rr.Code)
	}
	select {
	case ev := <-done:
		if ev.Repo != "acme/web" || ev.Number != 7 {
			t.Errorf("dispatched event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Review was not called for a signed reviewable event")
	}

	// Anything that must not dispatch uses a Review that fails the test if reached.
	never := &Handler{Secret: secret, Log: t.Logf, Review: func(context.Context, Event) { t.Error("Review must not be called") }}
	if rr := post(never, sign("wrong", body), "pull_request", body); rr.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status %d, want 401", rr.Code)
	}
	ignored := []byte(`{"action":"labeled","number":7,"repository":{"full_name":"acme/web"}}`)
	if rr := post(never, sign(secret, ignored), "pull_request", ignored); rr.Code != http.StatusNoContent {
		t.Errorf("ignored action: status %d, want 204", rr.Code)
	}
	// A moment for any wrongly-started goroutine to have run before the test ends.
	time.Sleep(20 * time.Millisecond)
}

// Only POST is accepted.
func TestHandlerRejectsGet(t *testing.T) {
	h := &Handler{Secret: "s", Review: func(context.Context, Event) { t.Error("GET must not review") }}
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rr.Code)
	}
}
