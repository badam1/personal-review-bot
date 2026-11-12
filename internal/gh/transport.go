package gh

import (
	"net/http"
	"time"
)

// callTimeout bounds one call to GitHub, its answer read in full: a large diff or a busy search takes seconds.
const callTimeout = time.Minute

// transport carries every call to GitHub. A laptop that sleeps or changes networks leaves its connection to GitHub dead
// but looking open, and a call on it waited minutes, until macOS gave up on the connection: after a wake the bar said
// "Not watching", and a review reading its PR just then failed. Now an HTTP/2 connection that has said nothing for 15
// seconds is pinged and dropped when the ping goes unanswered for 10 more, an answer must begin within 30 seconds, and
// a read that fails that way is sent once more, on a new connection.
func transport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second}
	return retryReads{t}
}

// retryReads sends a GET or HEAD once more when it failed before GitHub answered. A write (a comment, a review, an
// approval) is never sent twice: the first may have arrived.
type retryReads struct{ http.RoundTripper }

func (r retryReads) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.RoundTripper.RoundTrip(req)
	if err != nil && (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.Context().Err() == nil {
		return r.RoundTripper.RoundTrip(req)
	}
	return resp, err
}
