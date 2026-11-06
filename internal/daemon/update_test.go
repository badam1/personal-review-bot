package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/update"
)

// releases serves one release, v0.1.0-beta.2, with a binary for every platform.
type releases struct{ files map[string][]byte }

func newReleases(t *testing.T) releases {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "x/personal-review-bot", Mode: 0o755, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("NEW"))
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	var listing strings.Builder
	files := map[string][]byte{}
	for _, p := range []string{"darwin_arm64", "darwin_amd64", "linux_amd64", "linux_arm64"} {
		name := "personal-review-bot_0.1.0-beta.2_" + p + ".tar.gz"
		files[name] = buf.Bytes()
		listing.WriteString(hex.EncodeToString(sum[:]) + "  ./" + name + "\n")
	}
	files["checksums.txt"] = []byte(listing.String())
	return releases{files}
}

func (r releases) Releases(string) ([]gh.Release, error) {
	rel := gh.Release{Tag: "v0.1.0-beta.2", Prerelease: true, URL: "https://github.com/o/r/releases/tag/v0.1.0-beta.2"}
	for name := range r.files {
		rel.Assets = append(rel.Assets, gh.Asset{Name: name, URL: name})
	}
	return []gh.Release{rel}, nil
}

func (r releases) Download(url string, w io.Writer) error {
	_, err := w.Write(r.files[url])
	return err
}

func testUpdater(t *testing.T, current string) (*updater, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	exe := filepath.Join(t.TempDir(), "personal-review-bot")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := pipeline.Deps{Cfg: config.Config{UpdateRepo: "o/r"}, Store: st, Log: t.Logf}
	u := newUpdater(d, newReleases(t), exe)
	u.verify = func(context.Context, string, string) error { return nil }
	u.current = func() string { return current }
	return u, st, exe
}

func drained(u *updater) bool {
	select {
	case <-u.drain:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// An update installs the new binary, then drains: no new reviews, a restart once the running ones are done.
func TestUpdateStart(t *testing.T) {
	u, st, exe := testUpdater(t, "v0.1.0-beta.1")
	msg, err := u.Start()
	if err != nil || !strings.Contains(msg, "Updating to v0.1.0-beta.2") {
		t.Fatalf("Start = %q, %v", msg, err)
	}
	if !drained(u) {
		t.Fatalf("not drained; state %q", update.State(st))
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW" {
		t.Errorf("binary = %q", b)
	}
	if s := update.State(st); !strings.Contains(s, "v0.1.0-beta.2 installed: restarting") {
		t.Errorf("state = %q", s)
	}
	if _, err := u.Start(); err == nil || !strings.Contains(err.Error(), "already under way") {
		t.Errorf("a second update while one is under way: %v", err)
	}

	// A review still running holds the restart back; the worker takes no new one.
	u.running.Store(1)
	done := make(chan error, 1)
	go func() { done <- u.waitIdle(context.Background()) }()
	select {
	case <-done:
		t.Fatal("restarted while a review was running")
	case <-time.After(1500 * time.Millisecond):
	}
	u.running.Store(0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, Status: store.Queued})
	worker(context.Background(), u.d, u, 0)
	if r, _ := st.Get(id); r.Status != store.Queued {
		t.Errorf("a draining worker took a review: %s", r.Status)
	}
}

func TestUpdateUpToDate(t *testing.T) {
	u, st, exe := testUpdater(t, "v0.1.0-beta.2")
	msg, err := u.Start()
	if err != nil || msg != "Already up to date (v0.1.0-beta.2)." || u.draining() || u.busy.Load() {
		t.Errorf("Start = %q, %v; draining %v, busy %v", msg, err, u.draining(), u.busy.Load())
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Error("replaced the binary without a newer release")
	}
	if c := update.Load(st); c.Checked.IsZero() || c.Latest != nil {
		t.Errorf("check = %+v; want a check that found nothing newer", c)
	}
}

// A failed update says why and leaves the daemon as it was, ready to try again.
func TestUpdateFails(t *testing.T) {
	u, st, exe := testUpdater(t, "v0.1.0-beta.1")
	u.verify = func(context.Context, string, string) error { return errors.New("attestation not verified") }
	if _, err := u.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for u.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if u.busy.Load() || u.draining() {
		t.Fatalf("busy %v, draining %v after a failed update", u.busy.Load(), u.draining())
	}
	if s := update.State(st); !strings.Contains(s, "updating to v0.1.0-beta.2 failed: attestation not verified") {
		t.Errorf("state = %q", s)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Error("a failed update replaced the binary")
	}
}

// Check for updates looks right away and says what it found, without installing.
func TestCheckNow(t *testing.T) {
	u, st, exe := testUpdater(t, "v0.1.0-beta.1")
	if msg, err := u.CheckNow(); err != nil || msg != "v0.1.0-beta.2 is available." {
		t.Errorf("CheckNow = %q, %v", msg, err)
	}
	if c := update.Load(st); c.Latest == nil || c.Latest.Version != "v0.1.0-beta.2" {
		t.Errorf("the check isn't saved for the page: %+v", c)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" || u.busy.Load() {
		t.Error("checking installed something")
	}
	u.current = func() string { return "v0.1.0-beta.2" }
	if msg, err := u.CheckNow(); err != nil || msg != "Up to date (v0.1.0-beta.2)." {
		t.Errorf("CheckNow = %q, %v", msg, err)
	}
}

// A worker past PARALLEL leaves the queue alone until the Settings page raises it.
func TestWorkerFollowsParallel(t *testing.T) {
	u, st, _ := testUpdater(t, "v0.1.0")
	cfg := u.d.Cfg
	cfg.MaxParallel = 1
	u.d.Live = config.NewLive(cfg)
	id, _ := st.Insert(&store.Review{Repo: "o/r", Number: 1, Status: store.Queued})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	worker(ctx, u.d, u, 1)
	if r, _ := st.Get(id); r.Status != store.Queued {
		t.Errorf("the second worker took a review with PARALLEL=1: %s", r.Status)
	}
	if n := u.running.Load(); n != 0 {
		t.Errorf("%d reviews counted as running", n)
	}
}
