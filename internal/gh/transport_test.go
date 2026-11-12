package gh

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// failing fails its first sends the way a dead connection does, then answers.
type failing struct{ sends, fails int }

func (f *failing) RoundTrip(*http.Request) (*http.Response, error) {
	f.sends++
	if f.sends <= f.fails {
		return nil, errors.New("read: connection reset by peer")
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// A read that failed before GitHub answered goes once more; a write never does, nor a call whose time is up.
func TestRetryReads(t *testing.T) {
	cases := []struct {
		name          string
		method        string
		over          bool // the call's time is up
		fails, sends  int
		wantSucceeded bool
	}{
		{"a read on a dead connection, then a new one", http.MethodGet, false, 1, 2, true},
		{"a read that fails twice", http.MethodGet, false, 2, 2, false},
		{"a read answered at once", http.MethodGet, false, 0, 1, true},
		{"a write that may have arrived", http.MethodPost, false, 1, 1, false},
		{"a read whose time is up", http.MethodGet, true, 1, 1, false},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		if c.over {
			cancel()
		}
		req, _ := http.NewRequestWithContext(ctx, c.method, "https://api.github.com/user", nil)
		f := &failing{fails: c.fails}
		resp, err := retryReads{f}.RoundTrip(req)
		if f.sends != c.sends || (err == nil) != c.wantSucceeded {
			t.Errorf("%s: sent %d time(s), err %v; want %d, succeeded %v", c.name, f.sends, err, c.sends, c.wantSucceeded)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		cancel()
	}
}
