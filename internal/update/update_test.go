package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/store"
)

func TestCompare(t *testing.T) {
	// A git describe build ("-3-gabc1234", "-dirty") comes right after its tag and before the next version.
	ordered := []string{"v0.0.0-20261104125404-ede31dd39872", "v0.1.0-alpha", "v0.1.0-beta.1", "v0.1.0-beta.2",
		"v0.1.0-beta.2-dirty", "v0.1.0-beta.3", "v0.1.0-beta.3-1-g0123abc", "v0.1.0-beta.10", "v0.1.0-rc.1", "v0.1.0",
		"v0.1.0-2-gfeed123-dirty", "v0.1.1", "v0.2.0", "v1.0.0"}
	for i := range ordered {
		for j := range ordered {
			want := cmpInt(i, j)
			if got := Compare(ordered[i], ordered[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	if Compare("v1.2.3+build.7", "1.2.3") != 0 {
		t.Error("build metadata and the v prefix don't count")
	}
}

func release(tag string, pre bool, assets ...string) gh.Release {
	r := gh.Release{Tag: tag, Prerelease: pre, URL: "https://github.com/o/r/releases/tag/" + tag}
	for _, a := range assets {
		r.Assets = append(r.Assets, gh.Asset{Name: a, URL: "https://api.github.com/assets/" + a})
	}
	return r
}

func archiveName(tag string) string {
	return fmt.Sprintf("personal-review-bot_%s_darwin_arm64.tar.gz", strings.TrimPrefix(tag, "v"))
}

// A beta build is offered the newest release of any kind; a stable one only stable releases; a release without a
// binary for this machine, a draft, or one no newer than the running build, never.
func TestNewest(t *testing.T) {
	full := func(tag string, pre bool) gh.Release { return release(tag, pre, archiveName(tag), "checksums.txt") }
	draft := full("v0.3.0", false)
	draft.Draft = true
	releases := []gh.Release{
		draft,
		release("v0.2.1", false, "checksums.txt"), // no binary for darwin/arm64
		full("v0.2.0-beta.1", true),
		full("v0.1.0", false),
		full("v0.1.0-beta.2", true),
	}
	cases := []struct{ current, want string }{
		{"v0.1.0-beta.1", "v0.2.0-beta.1"},
		{"v0.1.0-beta.2-3-gabc1234", "v0.2.0-beta.1"},
		{"v0.0.9", "v0.1.0"},
		{"v0.1.0", ""},
		{"v0.2.0-beta.1", ""},
		{"dev (abc1234)", "v0.2.0-beta.1"},
	}
	for _, c := range cases {
		got, ok := Newest(releases, c.current, "darwin", "arm64")
		if (c.want == "") == ok || got.Version != c.want {
			t.Errorf("Newest for %s = %q, %v; want %q", c.current, got.Version, ok, c.want)
			continue
		}
		if ok && (got.Archive.Name != archiveName(c.want) || got.Checksums.Name != "checksums.txt") {
			t.Errorf("Newest for %s picked the wrong files: %+v", c.current, got)
		}
	}
}

func TestAvailable(t *testing.T) {
	rel := Release{Version: "v0.1.0-beta.3"}
	if _, ok := Available(Check{Latest: &rel}, "v0.1.0-beta.2"); !ok {
		t.Error("a newer release is available")
	}
	if _, ok := Available(Check{Latest: &rel}, "v0.1.0-beta.3"); ok {
		t.Error("after updating, the check's release is no longer newer")
	}
	if _, ok := Available(Check{}, "v0.1.0-beta.2"); ok {
		t.Error("no check, nothing available")
	}
}

// fakeGitHub serves a release's files from memory.
type fakeGitHub map[string][]byte

func (f fakeGitHub) Releases(string) ([]gh.Release, error) { return nil, nil }
func (f fakeGitHub) Download(url string, w io.Writer) error {
	b, ok := f[url]
	if !ok {
		return errors.New("404")
	}
	_, err := w.Write(b)
	return err
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// Install puts the archive's binary in place of the executable, and only when the checksum and attestation hold.
func TestInstall(t *testing.T) {
	const name = "personal-review-bot_0.1.0-beta.3_darwin_arm64.tar.gz"
	good := tarball(t, map[string]string{"personal-review-bot_0.1.0-beta.3_darwin_arm64/personal-review-bot": "NEW",
		"personal-review-bot_0.1.0-beta.3_darwin_arm64/README.md": "readme"})
	sum := sha256.Sum256(good)
	rel := Release{Version: "v0.1.0-beta.3", Archive: gh.Asset{Name: name, URL: "a"}, Checksums: gh.Asset{Name: "checksums.txt", URL: "c"}}
	attested := func(context.Context, string, string) error { return nil }

	setup := func() string {
		dir := t.TempDir()
		exe := filepath.Join(dir, "personal-review-bot")
		if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
			t.Fatal(err)
		}
		return exe
	}
	content := func(exe string) string { b, _ := os.ReadFile(exe); return string(b) }

	exe := setup()
	src := fakeGitHub{"a": good, "c": []byte(hex.EncodeToString(sum[:]) + "  ./" + name + "\n")}
	var verified string
	err := Install(context.Background(), src, "o/r", rel, exe, func(_ context.Context, archive, repo string) error {
		verified = filepath.Base(archive) + " from " + repo
		return nil
	})
	if err != nil || content(exe) != "NEW" || verified != name+" from o/r" {
		t.Fatalf("install: err %v, binary %q, verified %q", err, content(exe), verified)
	}
	if info, _ := os.Stat(exe); info.Mode().Perm()&0o100 == 0 {
		t.Error("the new binary isn't executable")
	}
	if left, _ := filepath.Glob(exe + ".new"); len(left) > 0 {
		t.Error("left a .new file behind")
	}

	failures := map[string]struct {
		src    fakeGitHub
		verify Verifier
		want   string
	}{
		"wrong checksum": {fakeGitHub{"a": good, "c": []byte(strings.Repeat("0", 64) + "  ./" + name)}, attested, "checksum"},
		"not listed":     {fakeGitHub{"a": good, "c": []byte(hex.EncodeToString(sum[:]) + "  ./other.tar.gz")}, attested, "no line for"},
		"not attested": {src, func(context.Context, string, string) error { return errors.New("no attestation") },
			"no attestation"},
		"no binary inside": {func() fakeGitHub {
			bad := tarball(t, map[string]string{"x/README.md": "readme"})
			s := sha256.Sum256(bad)
			return fakeGitHub{"a": bad, "c": []byte(hex.EncodeToString(s[:]) + " *" + name)}
		}(), attested, "has no personal-review-bot binary"},
		"download fails": {fakeGitHub{"c": src["c"]}, attested, "404"},
	}
	for what, f := range failures {
		exe := setup()
		err := Install(context.Background(), f.src, "o/r", rel, exe, f.verify)
		if err == nil || !strings.Contains(err.Error(), f.want) || content(exe) != "OLD" {
			t.Errorf("%s: err %v, binary %q; want an error about %q and the old binary", what, err, content(exe), f.want)
		}
	}
}

// Request asks the daemon's page to update, with its token, and returns what it said; nothing listening is
// ErrNoDaemon, so the CLI updates the binary itself.
func TestRequest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token, _ := st.CSRFToken()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/update" || r.FormValue("csrf") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.Redirect(w, r, "/?msg="+url.QueryEscape("Updating to v9: it restarts soon."), http.StatusSeeOther)
	}))
	defer srv.Close()
	cfg := config.Config{WebAddr: strings.TrimPrefix(srv.URL, "http://")}
	if msg, err := Request(cfg, st); err != nil || msg != "Updating to v9: it restarts soon." {
		t.Errorf("Request = %q, %v", msg, err)
	}
	srv.Close()
	if _, err := Request(cfg, st); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("nothing listening: %v, want ErrNoDaemon", err)
	}
}
