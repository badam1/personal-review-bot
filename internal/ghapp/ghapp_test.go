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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func genKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// parseKey reads the key GitHub issues (PKCS#1) and the PKCS#8 form too, and rejects anything else.
func TestParseKey(t *testing.T) {
	k, pkcs1 := genKey(t)
	if _, err := parseKey(pkcs1); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}
	pkcs8Bytes, _ := x509.MarshalPKCS8PrivateKey(k)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})
	if _, err := parseKey(pkcs8); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}
	if _, err := parseKey([]byte("not a pem")); err == nil {
		t.Error("non-PEM input should error")
	}
	if _, err := New("", pkcs1); err == nil {
		t.Error("empty app id should error")
	}
}

// The app JWT is signed by the app key and carries the documented claims: iss = app id, iat backdated for skew, and
// exp no more than GitHub's 10-minute ceiling.
func TestJWTClaimsAndSignature(t *testing.T) {
	key, pemBytes := genKey(t)
	a, err := New("app-123", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	jwt, err := a.jwt()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3", len(parts))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify against the app's public key: %v", err)
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &c); err != nil {
		t.Fatal(err)
	}
	if c.Iss != "app-123" {
		t.Errorf("iss = %q, want app-123", c.Iss)
	}
	if c.Iat > now.Unix() {
		t.Errorf("iat %d is not backdated (now %d)", c.Iat, now.Unix())
	}
	if span := c.Exp - c.Iat; span > 10*60 {
		t.Errorf("exp-iat = %ds, over GitHub's 600s ceiling", span)
	}
}

// InstallationToken mints an installation token with the app JWT at the documented path, then serves it from cache
// until near expiry.
func TestInstallationTokenMintAndCache(t *testing.T) {
	key, pemBytes := genKey(t)
	var calls int
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"token":"ghs_minted","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	}))
	defer srv.Close()
	a, err := New("123", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	a.api, a.http = srv.URL, srv.Client()

	tok, err := a.InstallationToken(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_minted" {
		t.Errorf("token = %q, want ghs_minted", tok)
	}
	if gotPath != "/app/installations/42/access_tokens" {
		t.Errorf("path = %q", gotPath)
	}
	jwt, ok := strings.CutPrefix(gotAuth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want Bearer <jwt>", gotAuth)
	}
	sum := sha256.Sum256([]byte(strings.Join(strings.Split(jwt, ".")[:2], ".")))
	sig, _ := base64.RawURLEncoding.DecodeString(strings.Split(jwt, ".")[2])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("mint request's JWT not signed by the app key: %v", err)
	}
	if _, err := a.InstallationToken(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("made %d mint calls, want 1 (second served from cache)", calls)
	}
}

// A token within a minute of expiry is re-minted rather than reused.
func TestInstallationTokenRefreshesNearExpiry(t *testing.T) {
	_, pemBytes := genKey(t)
	now := time.Unix(1_700_000_000, 0)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"token":"ghs_x","expires_at":"`+now.Add(90*time.Second).UTC().Format(time.RFC3339)+`"}`)
	}))
	defer srv.Close()
	a, _ := New("1", pemBytes)
	a.api, a.http = srv.URL, srv.Client()
	a.now = func() time.Time { return now }

	if _, err := a.InstallationToken(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	now = now.Add(60 * time.Second) // now within the 1-minute refresh window of the 90s expiry
	if _, err := a.InstallationToken(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("made %d mint calls, want 2 (re-minted near expiry)", calls)
	}
}

// Login reads the app identity and forms the bot login "<slug>[bot]", caching it.
func TestLogin(t *testing.T) {
	_, pemBytes := genKey(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/app" {
			t.Errorf("path = %q, want /app", r.URL.Path)
		}
		io.WriteString(w, `{"slug":"acme-reviewer"}`)
	}))
	defer srv.Close()
	a, _ := New("1", pemBytes)
	a.api, a.http = srv.URL, srv.Client()

	login, err := a.Login(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if login != "acme-reviewer[bot]" {
		t.Errorf("login = %q, want acme-reviewer[bot]", login)
	}
	if _, err := a.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("made %d identity calls, want 1 (cached)", calls)
	}
}

// The installation transport sets the installation token on each request without mutating the caller's request.
func TestInstallTransportSetsToken(t *testing.T) {
	_, pemBytes := genKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"token":"ghs_rt","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	}))
	defer srv.Close()
	a, _ := New("1", pemBytes)
	a.api, a.http = srv.URL, srv.Client()

	var gotAuth string
	base := rtFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})
	rt := a.Transport(99)(base)

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/o/r", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "token ghs_rt" {
		t.Errorf("downstream Authorization = %q, want token ghs_rt", gotAuth)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("the caller's request must not be mutated")
	}
}
