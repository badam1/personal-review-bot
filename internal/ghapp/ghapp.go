// Package ghapp authenticates as a GitHub App: it signs a short-lived RS256 JWT with the App's private key, exchanges
// it for per-installation access tokens, and hands out a transport that keeps each installation's token fresh. The
// App's private key is the only long-lived secret; installation tokens last about an hour and are never written down.
//
// What is exercised by tests here: the PEM key parsing, the JWT (its claims and its signature, checked against the
// public key), the token-mint and identity requests (against a stub), token caching and refresh, and the transport's
// per-request header. What is NOT exercised here — because it needs a registered App and GitHub's own servers — is the
// live acceptance of the JWT and the real token it returns; that request is built to GitHub's documented contract.
package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// App is a configured GitHub App: its id (the JWT issuer — the App ID or client ID) and RSA private key. It mints and
// caches per-installation tokens and learns its own bot login once. Safe for concurrent use.
type App struct {
	id   string
	key  *rsa.PrivateKey
	http *http.Client
	api  string           // API base, e.g. https://api.github.com (a seam for tests)
	now  func() time.Time // clock (a seam for tests)

	mu     sync.Mutex
	tokens map[int64]token // per-installation token cache
	login  string          // "<app-slug>[bot]", fetched once
}

type token struct {
	value   string
	expires time.Time
}

// New builds an App from its id (App ID or client ID) and the PEM-encoded RSA private key GitHub issued (the .pem).
func New(id string, pemKey []byte) (*App, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("github app id is empty")
	}
	key, err := parseKey(pemKey)
	if err != nil {
		return nil, err
	}
	return &App{
		id:     id,
		key:    key,
		http:   &http.Client{Timeout: 30 * time.Second},
		api:    "https://api.github.com",
		now:    time.Now,
		tokens: map[int64]token{},
	}, nil
}

// parseKey reads the App's private key. GitHub issues PKCS#1 ("RSA PRIVATE KEY"); PKCS#8 is accepted too.
func parseKey(pemKey []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, fmt.Errorf("github app key: not PEM-encoded")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github app key: not an RSA key")
	}
	return rk, nil
}

// jwt is a short-lived RS256 app token. GitHub accepts it for at most 10 minutes, so it expires at 9 and backdates iat
// a minute for clock skew. It authenticates only the app-level calls (mint a token, read the app identity).
func (a *App) jwt() (string, error) {
	now := a.now()
	header := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims := b64(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%q}`, now.Add(-time.Minute).Unix(), now.Add(9*time.Minute).Unix(), a.id))
	signing := header + "." + claims
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// InstallationToken returns a valid access token for one installation, minting a new one (POST
// /app/installations/{id}/access_tokens with the app JWT) when the cache has none or it is within a minute of expiry.
// Tokens last about an hour and a review takes minutes, so one mint serves many calls.
func (a *App) InstallationToken(ctx context.Context, installID int64) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.tokens[installID]; ok && a.now().Before(t.expires.Add(-time.Minute)) {
		return t.value, nil
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.appCall(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installID), http.StatusCreated, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("mint installation %d token: empty token", installID)
	}
	a.tokens[installID] = token{value: out.Token, expires: out.ExpiresAt}
	return out.Token, nil
}

// Login is the App's bot login, "<app-slug>[bot]", read once via GET /app. Comments the App posts are authored by this
// login, so the review path uses it to recognize the bot's own threads and approvals.
func (a *App) Login(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.login != "" {
		return a.login, nil
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := a.appCall(ctx, http.MethodGet, "/app", http.StatusOK, &out); err != nil {
		return "", err
	}
	if out.Slug == "" {
		return "", fmt.Errorf("github app identity: empty slug")
	}
	a.login = out.Slug + "[bot]"
	return a.login, nil
}

// appCall makes one app-JWT-authenticated request and decodes a wantStatus response into out.
func (a *App) appCall(ctx context.Context, method, path string, wantStatus int, out any) error {
	jwt, err := a.jwt()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// Transport returns a wrapper that authenticates each request as the given installation, minting and caching the
// token as needed. It wraps base (gh's own transport), so retries and keep-alive are preserved, and refreshes the
// token itself so a long-running client keeps working past the token's ~1h expiry.
func (a *App) Transport(installID int64) func(http.RoundTripper) http.RoundTripper {
	return func(base http.RoundTripper) http.RoundTripper {
		return &installTransport{app: a, installID: installID, base: base}
	}
}

type installTransport struct {
	app       *App
	installID int64
	base      http.RoundTripper
}

func (t *installTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.app.InstallationToken(req.Context(), t.installID)
	if err != nil {
		return nil, err
	}
	r2 := req.Clone(req.Context()) // a RoundTripper must not mutate the caller's request
	r2.Header.Set("Authorization", "token "+tok)
	return t.base.RoundTrip(r2)
}
