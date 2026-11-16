package gh

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// captureRT records the Authorization header of the request it carries and answers every call with body, so a test
// can see how a client authenticates without reaching GitHub.
type captureRT struct {
	auth string
	body string
}

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.auth = r.Header.Get("Authorization")
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Request:    r,
	}, nil
}

// A token client authenticates every call as that token — go-gh sends `Authorization: token <token>` over our
// transport — and learns its own login, so a channel can act under a distinct GitHub identity.
func TestNewWithTokenAuthenticates(t *testing.T) {
	rt := &captureRT{body: `{"login":"acme-bot"}`}
	c, err := newClientWith("ghs_secret123", rt)
	if err != nil {
		t.Fatal(err)
	}
	if c.Login != "acme-bot" {
		t.Errorf("login = %q, want acme-bot", c.Login)
	}
	if rt.auth != "token ghs_secret123" {
		t.Errorf("Authorization = %q, want %q", rt.auth, "token ghs_secret123")
	}
}

// NewWithToken rejects an empty token rather than silently falling back to the ambient login.
func TestNewWithTokenEmpty(t *testing.T) {
	if _, err := NewWithToken(""); err == nil {
		t.Error("NewWithToken(\"\") should error")
	}
}
