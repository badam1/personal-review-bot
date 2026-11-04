// Package gh talks to GitHub with the user's existing `gh` login (via go-gh), so no separate token is needed.
package gh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

type Client struct {
	rest  *api.RESTClient
	diff  *api.RESTClient
	gql   *api.GraphQLClient
	dl    *http.Client // release asset downloads
	Login string
}

type PR struct {
	Repo      string // owner/name
	Number    int
	URL       string
	Title     string
	Body      string
	Author    string
	HeadSHA   string
	HeadRef   string
	BaseRef   string
	Draft     bool
	Additions int
	Deletions int
	Files     int
	// RequestedAt is when a review was last requested from you directly (re-requests move it forward).
	RequestedAt time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time // last activity: pushed commits, comments, reviews, edits
}

type File struct {
	Path      string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// Comment is an inline review comment on the new side of the diff.
type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

func New() (*Client, error) {
	rest, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("GitHub login (run `gh auth login`): %w", err)
	}
	diff, err := api.NewRESTClient(api.ClientOptions{Headers: map[string]string{"Accept": "application/vnd.github.v3.diff"}})
	if err != nil {
		return nil, err
	}
	gql, err := api.DefaultGraphQLClient()
	if err != nil {
		return nil, err
	}
	dl, err := api.NewHTTPClient(api.ClientOptions{Headers: map[string]string{"Accept": "application/octet-stream"}})
	if err != nil {
		return nil, err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := rest.Get("user", &u); err != nil {
		return nil, fmt.Errorf("GitHub user: %w", err)
	}
	return &Client{rest: rest, diff: diff, gql: gql, dl: dl, Login: u.Login}, nil
}

var prURL = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/(\d+)`)

// ParseURL turns https://github.com/owner/repo/pull/123 into ("owner/repo", 123).
func ParseURL(u string) (string, int, error) {
	m := prURL.FindStringSubmatch(u)
	if m == nil {
		return "", 0, fmt.Errorf("not a GitHub PR URL: %s", u)
	}
	n, _ := strconv.Atoi(m[2])
	return m[1], n, nil
}

// RequestedFromMe lists open PRs where a review is requested from the logged-in user directly — not via a team.
func (c *Client) RequestedFromMe(org string) ([]PR, error) {
	q := "is:pr is:open archived:false user-review-requested:" + c.Login
	if org != "" {
		q += " org:" + org
	}
	const query = `query($q: String!) { search(query: $q, type: ISSUE, first: 50) { nodes { ... on PullRequest {
	  url number title isDraft createdAt updatedAt headRefOid additions deletions changedFiles author { login } repository { nameWithOwner }
	  timelineItems(itemTypes: [REVIEW_REQUESTED_EVENT], last: 20) { nodes { ... on ReviewRequestedEvent {
	    createdAt requestedReviewer { ... on User { login } } } } } } } } }`
	var resp struct {
		Search struct {
			Nodes []struct {
				URL          string                 `json:"url"`
				Number       int                    `json:"number"`
				Title        string                 `json:"title"`
				IsDraft      bool                   `json:"isDraft"`
				CreatedAt    time.Time              `json:"createdAt"`
				UpdatedAt    time.Time              `json:"updatedAt"`
				HeadRefOid   string                 `json:"headRefOid"`
				Additions    int                    `json:"additions"`
				Deletions    int                    `json:"deletions"`
				ChangedFiles int                    `json:"changedFiles"`
				Author       struct{ Login string } `json:"author"`
				Repository   struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
				TimelineItems struct {
					Nodes []struct {
						CreatedAt         time.Time              `json:"createdAt"`
						RequestedReviewer struct{ Login string } `json:"requestedReviewer"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"nodes"`
		} `json:"search"`
	}
	if err := c.gql.Do(query, map[string]interface{}{"q": q}, &resp); err != nil {
		return nil, err
	}
	var out []PR
	for _, n := range resp.Search.Nodes {
		if n.Number == 0 {
			continue
		}
		pr := PR{Repo: n.Repository.NameWithOwner, Number: n.Number, URL: n.URL, Title: n.Title,
			Author: n.Author.Login, HeadSHA: n.HeadRefOid, Draft: n.IsDraft, Additions: n.Additions,
			Deletions: n.Deletions, Files: n.ChangedFiles, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt}
		for _, ev := range n.TimelineItems.Nodes {
			if ev.RequestedReviewer.Login == c.Login && ev.CreatedAt.After(pr.RequestedAt) {
				pr.RequestedAt = ev.CreatedAt
			}
		}
		out = append(out, pr)
	}
	return out, nil
}

// Conversation renders the PR's discussion for the reviewers: general comments, reviews, and inline threads with
// their resolved/outdated state. Comments this bot posted are marked "[personal-review-bot]" (detected by the watermark).
func (c *Client) Conversation(repo string, number int, watermark string) (string, error) {
	owner, name, _ := strings.Cut(repo, "/")
	const query = `query($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) {
	  pullRequest(number: $number) {
	    comments(last: 60) { nodes { author { login } body createdAt } }
	    reviews(last: 40) { nodes { author { login } state body submittedAt } }
	    reviewThreads(last: 80) { nodes { isResolved isOutdated path line originalLine
	      comments(first: 20) { nodes { author { login } body createdAt } } } } } } }`
	type comment struct {
		Author    struct{ Login string } `json:"author"`
		Body      string                 `json:"body"`
		CreatedAt time.Time              `json:"createdAt"`
	}
	var resp struct {
		Repository struct {
			PullRequest struct {
				Comments struct{ Nodes []comment } `json:"comments"`
				Reviews  struct {
					Nodes []struct {
						Author      struct{ Login string } `json:"author"`
						State       string                 `json:"state"`
						Body        string                 `json:"body"`
						SubmittedAt time.Time              `json:"submittedAt"`
					}
				} `json:"reviews"`
				ReviewThreads struct {
					Nodes []struct {
						IsResolved   bool                      `json:"isResolved"`
						IsOutdated   bool                      `json:"isOutdated"`
						Path         string                    `json:"path"`
						Line         int                       `json:"line"`
						OriginalLine int                       `json:"originalLine"`
						Comments     struct{ Nodes []comment } `json:"comments"`
					}
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]interface{}{"owner": owner, "name": name, "number": number}
	if err := c.gql.Do(query, vars, &resp); err != nil {
		return "", err
	}

	clean := func(login, body string) string {
		who := "@" + login
		if watermark != "" && strings.Contains(body, watermark) {
			who += " [personal-review-bot]"
			if i := strings.LastIndex(body, "<sub>"); i >= 0 {
				body = body[:i]
			}
		}
		body = strings.Join(strings.Fields(body), " ")
		if r := []rune(body); len(r) > 600 {
			body = string(r[:600]) + "…"
		}
		return who + ": " + body
	}
	pr := resp.Repository.PullRequest
	var b strings.Builder
	if len(pr.Comments.Nodes) > 0 {
		b.WriteString("General comments:\n")
		for _, c := range pr.Comments.Nodes {
			fmt.Fprintf(&b, "- (%s) %s\n", c.CreatedAt.Format("Jan 2 15:04"), clean(c.Author.Login, c.Body))
		}
	}
	var reviews []string
	for _, r := range pr.Reviews.Nodes {
		if strings.TrimSpace(r.Body) == "" && r.State == "COMMENTED" {
			continue // just the container of inline comments, shown below
		}
		reviews = append(reviews, fmt.Sprintf("- (%s) %s %s", r.SubmittedAt.Format("Jan 2 15:04"), r.State, clean(r.Author.Login, r.Body)))
	}
	if len(reviews) > 0 {
		b.WriteString("\nReviews:\n" + strings.Join(reviews, "\n") + "\n")
	}
	if len(pr.ReviewThreads.Nodes) > 0 {
		b.WriteString("\nInline threads:\n")
		for _, t := range pr.ReviewThreads.Nodes {
			line := t.Line
			if line == 0 {
				line = t.OriginalLine
			}
			state := "open"
			if t.IsResolved {
				state = "resolved"
			}
			if t.IsOutdated {
				state += ", outdated (code changed since)"
			}
			fmt.Fprintf(&b, "- %s:%d [%s]\n", t.Path, line, state)
			for _, c := range t.Comments.Nodes {
				fmt.Fprintf(&b, "    %s\n", clean(c.Author.Login, c.Body))
			}
		}
	}
	out := b.String()
	if out == "" {
		return "(no comments yet)", nil
	}
	if r := []rune(out); len(r) > 40_000 { // keep the most recent part
		out = "… (older conversation truncated)\n" + string(r[len(r)-40_000:])
	}
	return out, nil
}

func (c *Client) GetPR(repo string, number int) (PR, error) {
	var p struct {
		HTMLURL      string                 `json:"html_url"`
		Title        string                 `json:"title"`
		Body         string                 `json:"body"`
		Draft        bool                   `json:"draft"`
		Additions    int                    `json:"additions"`
		Deletions    int                    `json:"deletions"`
		ChangedFiles int                    `json:"changed_files"`
		User         struct{ Login string } `json:"user"`
		Head         struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.rest.Get(fmt.Sprintf("repos/%s/pulls/%d", repo, number), &p); err != nil {
		return PR{}, err
	}
	return PR{Repo: repo, Number: number, URL: p.HTMLURL, Title: p.Title, Body: p.Body, Author: p.User.Login,
		HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, Draft: p.Draft, Additions: p.Additions,
		Deletions: p.Deletions, Files: p.ChangedFiles}, nil
}

// Files returns the changed files with their patches (GitHub caps this at 3000 files).
func (c *Client) Files(repo string, number int) ([]File, error) {
	var all []File
	for page := 1; page <= 30; page++ {
		var batch []File
		if err := c.rest.Get(fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page), &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// Diff returns the PR's unified diff.
func (c *Client) Diff(repo string, number int) (string, error) {
	resp, err := c.diff.Request("GET", fmt.Sprintf("repos/%s/pulls/%d", repo, number), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	return string(b), err
}

// Review is one review on a PR, as the REST API lists it.
type Review struct {
	User  struct{ Login string } `json:"user"`
	State string                 `json:"state"`
	Body  string                 `json:"body"`
}

// ApprovedWith reports whether you already approved the PR with a review saying text (case aside, 👍 and :+1: alike).
// An approval that a later push dismissed counts too: it was said.
func (c *Client) ApprovedWith(repo string, number int, text string) (bool, error) {
	var all []Review
	for page := 1; page <= 10; page++ {
		var batch []Review
		if err := c.rest.Get(fmt.Sprintf("repos/%s/pulls/%d/reviews?per_page=100&page=%d", repo, number, page), &batch); err != nil {
			return false, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return saidIn(all, c.Login, text), nil
}

func saidIn(reviews []Review, login, text string) bool {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "👍", ":+1:") }
	for _, r := range reviews {
		if r.User.Login == login && (r.State == "APPROVED" || r.State == "DISMISSED") && strings.Contains(norm(r.Body), norm(text)) {
			return true
		}
	}
	return false
}

// PostReview submits one review. event is COMMENT or APPROVE; the bot never uses REQUEST_CHANGES.
func (c *Client) PostReview(repo string, number int, commitSHA, event, body string, comments []Comment) (string, error) {
	if comments == nil {
		comments = []Comment{}
	}
	payload, err := json.Marshal(map[string]any{"commit_id": commitSHA, "event": event, "body": body, "comments": comments})
	if err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err = c.rest.Post(fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number), bytes.NewReader(payload), &out)
	return out.HTMLURL, err
}

// Release is a published GitHub release and its files.
type Release struct {
	Tag        string    `json:"tag_name"`
	URL        string    `json:"html_url"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []Asset   `json:"assets"`
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // the API address, which Download fetches
	Size int64  `json:"size"`
}

// Releases lists repo's latest 30 releases, newest first.
func (c *Client) Releases(repo string) ([]Release, error) {
	var out []Release
	err := c.rest.Get(fmt.Sprintf("repos/%s/releases?per_page=30", repo), &out)
	return out, err
}

// Download writes a release asset to w.
func (c *Client) Download(assetURL string, w io.Writer) error {
	resp, err := c.dl.Get(assetURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", assetURL, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
