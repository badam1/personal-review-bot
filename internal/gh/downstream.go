package gh

import (
	"strings"

	"github.com/badam1/personal-review-bot/internal/downstream"
)

// ReviewThreadStates reads a PR's review threads for the downstream poller: each thread's location, whether it is
// resolved, and the reactions and reply count on its first comment (the bot's). It is gh's implementation of
// downstream.Checker, reached through a thin adapter in the daemon. One GraphQL call, the latest 100 threads with
// their first 100 comments; a thread beyond that is not read, which only leaves a finding Unknown, never wrong.
func (c *Client) ReviewThreadStates(repo string, number int) ([]downstream.Thread, error) {
	owner, name, _ := strings.Cut(repo, "/")
	const query = `query($owner: String!, $name: String!, $number: Int!) { repository(owner: $owner, name: $name) {
	  pullRequest(number: $number) { reviewThreads(first: 100) { nodes {
	    isResolved path line originalLine
	    comments(first: 100) { totalCount nodes { reactionGroups { content reactors { totalCount } } } } } } } } }`
	var resp struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						IsResolved   bool   `json:"isResolved"`
						Path         string `json:"path"`
						Line         int    `json:"line"`
						OriginalLine int    `json:"originalLine"`
						Comments     struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
								ReactionGroups []struct {
									Content  string `json:"content"`
									Reactors struct {
										TotalCount int `json:"totalCount"`
									} `json:"reactors"`
								} `json:"reactionGroups"`
							} `json:"nodes"`
						} `json:"comments"`
					}
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.gql.Do(query, map[string]any{"owner": owner, "name": name, "number": number}, &resp); err != nil {
		return nil, err
	}
	var out []downstream.Thread
	for _, n := range resp.Repository.PullRequest.ReviewThreads.Nodes {
		st := downstream.State{Exists: len(n.Comments.Nodes) > 0, Resolved: n.IsResolved}
		if st.Exists {
			for _, rg := range n.Comments.Nodes[0].ReactionGroups {
				switch rg.Content {
				case "THUMBS_UP":
					st.ThumbsUp = rg.Reactors.TotalCount
				case "THUMBS_DOWN":
					st.ThumbsDown = rg.Reactors.TotalCount
				}
			}
			if n.Comments.TotalCount > 1 {
				st.Replies = n.Comments.TotalCount - 1
			}
		}
		out = append(out, downstream.Thread{Path: n.Path, Line: n.Line, OrigLine: n.OriginalLine, State: st})
	}
	return out, nil
}
