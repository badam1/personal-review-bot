package gh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Account is the GitHub account the bot acts as: who its comments and approvals come from.
type Account struct {
	Login     string
	Name      string
	AvatarURL string
	HTMLURL   string
	Company   string
	Created   time.Time
	Orgs      []string // the organisations the account belongs to, as GitHub lists them
}

// Account reads the logged-in account. The organisations need the read:org scope, which `gh auth login` asks for
// by default; without it the rest of the account still comes back and Orgs is empty.
func (c *Client) Account() (Account, error) {
	var u struct {
		Login     string    `json:"login"`
		Name      string    `json:"name"`
		AvatarURL string    `json:"avatar_url"`
		HTMLURL   string    `json:"html_url"`
		Company   string    `json:"company"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := c.rest.Get("user", &u); err != nil {
		return Account{}, err
	}
	a := Account{Login: u.Login, Name: u.Name, AvatarURL: u.AvatarURL, HTMLURL: u.HTMLURL,
		Company: strings.TrimSpace(u.Company), Created: u.CreatedAt}
	var orgs []struct {
		Login string `json:"login"`
	}
	if err := c.rest.Get("user/orgs?per_page=100", &orgs); err == nil {
		for _, o := range orgs {
			a.Orgs = append(a.Orgs, o.Login)
		}
	}
	return a, nil
}

const (
	// avatarHostname is the one host GitHub serves avatars from. The wider githubusercontent.com suffix would also
	// admit raw.githubusercontent.com and friends, which serve whatever a user put in a repository.
	avatarHostname = "avatars.githubusercontent.com"
	avatarMax      = 4 << 20 // the largest picture served, in bytes
)

// avatarHost reports whether raw is an address an avatar is served from. The address comes from the API rather
// than from anyone's input, so this is a guard rather than a filter: it keeps a redirect or a changed field from
// turning the fetch below into a request to somewhere else.
func avatarHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	return u.Host == avatarHostname
}

// avatarPictures are the media types served as a picture. SVG is left out on purpose: it can carry script, and
// serving it from the bot's own origin would put that script inside the page's origin rather than GitHub's.
var avatarPictures = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// Avatar downloads the account's picture and returns it with its media type.
//
// It uses a plain client rather than the API one on purpose: the avatar host is not the API, and the API client
// carries your gh token. The page then serves these bytes itself, so its content-security-policy stays at
// img-src 'self' and the browser showing your profile asks GitHub for nothing.
func Avatar(ctx context.Context, rawURL string) ([]byte, string, error) {
	if !avatarHost(rawURL) {
		return nil, "", fmt.Errorf("not a GitHub avatar address: %s", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if !avatarHost(r.URL.String()) {
			return fmt.Errorf("avatar redirected off GitHub: %s", r.URL.Host)
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("avatar: %s", resp.Status)
	}
	return picture(resp)
}

// picture reads a reply as a picture: one of the handful of types a browser draws without running anything, and
// no larger than the cap.
func picture(resp *http.Response) ([]byte, string, error) {
	media, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	media = strings.ToLower(strings.TrimSpace(media))
	if !slices.Contains(avatarPictures, media) {
		return nil, "", fmt.Errorf("avatar is not a picture: %s", media)
	}
	// One byte over the cap is read so that a picture larger than it is refused rather than silently cut short:
	// io.LimitReader stops without an error, and the half-picture would then be cached and served as a whole one.
	b, err := io.ReadAll(io.LimitReader(resp.Body, avatarMax+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > avatarMax {
		return nil, "", fmt.Errorf("avatar is larger than %d MB", avatarMax>>20)
	}
	return b, media, nil
}
