// Package update finds newer releases of personal-review-bot and installs one: the release's binary for this OS and
// CPU, checked against the release's checksums and its GitHub build attestation, put in place of the running
// executable. The daemon then restarts into it.
package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/gh"
	"github.com/badam1/personal-review-bot/internal/store"
)

// Release is a release this build can update to.
type Release struct {
	Version   string    `json:"version"` // its tag, e.g. v0.1.0-beta.3
	URL       string    `json:"url"`     // its page, with the notes
	Archive   gh.Asset  `json:"archive"` // the binary for this OS and CPU
	Checksums gh.Asset  `json:"checksums"`
	Published time.Time `json:"published"`
}

// Source is the GitHub API, as far as updates need it.
type Source interface {
	Releases(repo string) ([]gh.Release, error)
	Download(assetURL string, w io.Writer) error
}

// Newest returns the newest release that is newer than current and has a binary for goos/goarch. Pre-releases are
// offered only while current is one (or isn't a version at all, like a dev build); drafts never.
func Newest(releases []gh.Release, current, goos, goarch string) (Release, bool) {
	cur, known := parse(current)
	pre := !known || len(cur.pre) > 0
	var best Release
	var bestV version
	found := false
	for _, r := range releases {
		v, ok := parse(r.Tag)
		if !ok || r.Draft || (r.Prerelease || len(v.pre) > 0) && !pre {
			continue
		}
		if known && compare(v, cur) <= 0 {
			continue
		}
		if found && compare(v, bestV) <= 0 {
			continue
		}
		archive := fmt.Sprintf("personal-review-bot_%s_%s_%s.tar.gz", strings.TrimPrefix(r.Tag, "v"), goos, goarch)
		var rel Release
		for _, a := range r.Assets {
			switch a.Name {
			case archive:
				rel.Archive = a
			case "checksums.txt":
				rel.Checksums = a
			}
		}
		if rel.Archive.URL == "" || rel.Checksums.URL == "" {
			continue
		}
		rel.Version, rel.URL, rel.Published = r.Tag, r.URL, r.Published
		best, bestV, found = rel, v, true
	}
	return best, found
}

// Verifier checks that an archive was built by repo's release workflow.
type Verifier func(ctx context.Context, archive, repo string) error

// Attested checks the archive's GitHub build attestation with `gh attestation verify`: it must have been built by
// repo's own release workflow.
func Attested(ctx context.Context, archive, repo string) error {
	out, err := exec.CommandContext(ctx, "gh", "attestation", "verify", archive, "--repo", repo,
		"--signer-workflow", repo+"/.github/workflows/release.yml").CombinedOutput()
	if err != nil {
		return fmt.Errorf("build attestation not verified (needs gh 2.49 or later): %v: %s", err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// Install downloads rel's archive, checks its checksum and attestation, and puts its binary in place of exe. The new
// binary is written next to exe and renamed over it, so exe is either the old binary or the whole new one; a process
// running the old one carries on until it restarts. step, if not nil, hears each step as it starts.
func Install(ctx context.Context, src Source, repo string, rel Release, exe string, verify Verifier, step func(string)) error {
	if step == nil {
		step = func(string) {}
	}
	tmp, err := os.MkdirTemp("", "personal-review-bot-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	step(fmt.Sprintf("downloading %s (%.1f MB)", rel.Version, float64(rel.Archive.Size)/1e6))
	var sums strings.Builder
	if err := src.Download(rel.Checksums.URL, &sums); err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	want, ok := checksum(sums.String(), rel.Archive.Name)
	if !ok {
		return fmt.Errorf("checksums.txt has no line for %s", rel.Archive.Name)
	}
	archive := filepath.Join(tmp, rel.Archive.Name)
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	h := sha256.New()
	err = src.Download(rel.Archive.URL, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", rel.Archive.Name, err)
	}
	step(fmt.Sprintf("checking %s's checksum and build attestation", rel.Version))
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: checksum %s, want %s", rel.Archive.Name, got, want)
	}
	if err := verify(ctx, archive, repo); err != nil {
		return err
	}
	step("installing " + rel.Version)

	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	next := exe + ".new"
	if err := extract(archive, next); err != nil {
		_ = os.Remove(next)
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Remove(next)
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}

// checksum finds name's sha256 in a sha256sum listing ("<hex>  ./name" or "<hex> *name").
func checksum(listing, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(listing))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./") == name {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// extract writes the archive's personal-review-bot binary to dst, executable.
func extract(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s has no personal-review-bot binary", filepath.Base(archive))
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "personal-review-bot" {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("can't write next to the running binary: %w", err)
		}
		_, err = io.Copy(out, io.LimitReader(tr, 512<<20))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
}

// Check is the latest look for a newer release, kept in the database for the page and the TUI.
type Check struct {
	Checked time.Time `json:"checked"`
	Latest  *Release  `json:"latest,omitempty"` // nil: up to date
	Err     string    `json:"error,omitempty"`
}

// Save stores a check.
func Save(st *store.Store, c Check) {
	if js, err := json.Marshal(c); err == nil {
		_ = st.SetMeta("update_check", string(js))
	}
}

// Load returns the latest check (zero if there was none).
func Load(st *store.Store) Check {
	var c Check
	_ = json.Unmarshal([]byte(st.Meta("update_check")), &c)
	return c
}

// Done is the last update that finished, which the page and the TUI mention for a while.
type Done struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
}

// Restarting records, just before the daemon restarts into an update, the version it is leaving.
func Restarting(st *store.Store, from string) { _ = st.SetMeta("update_from", from) }

// Started is called when the daemon starts: it records the version it runs, for the TUI to compare with its own, and,
// after an update's restart, the update as done. It returns that update (false when this start wasn't one).
func Started(st *store.Store, current string) (Done, bool) {
	_ = st.SetMeta("daemon_version", current)
	SetState(st, "")
	from := st.Meta("update_from")
	if from == "" {
		return Done{}, false
	}
	_ = st.SetMeta("update_from", "")
	d := Done{From: from, To: current, At: time.Now()}
	if js, err := json.Marshal(d); err == nil {
		_ = st.SetMeta("update_done", string(js))
	}
	return d, true
}

// LastDone returns the last finished update if it was within the last hour.
func LastDone(st *store.Store, now time.Time) (Done, bool) {
	var d Done
	if json.Unmarshal([]byte(st.Meta("update_done")), &d) != nil || now.Sub(d.At) > time.Hour {
		return Done{}, false
	}
	return d, true
}

// DaemonVersion is the version the daemon last started as ("" before one did).
func DaemonVersion(st *store.Store) string { return st.Meta("daemon_version") }

// Busy reports whether a state is an update under way (not empty, not a failure).
func Busy(state string) bool { return state != "" && !strings.Contains(state, "failed") }

// State is what an update in progress is doing, e.g. "downloading v0.1.0-beta.3" ("" = none).
func State(st *store.Store) string { return st.Meta("update_state") }

func SetState(st *store.Store, s string) { _ = st.SetMeta("update_state", s) }

// Available returns the release the latest check found, if it is newer than current.
func Available(c Check, current string) (Release, bool) {
	if c.Latest == nil {
		return Release{}, false
	}
	v, _ := parse(c.Latest.Version)
	if cur, ok := parse(current); ok && compare(v, cur) <= 0 {
		return Release{}, false
	}
	return *c.Latest, true
}

// Compare orders two versions by SemVer precedence (-1, 0, 1); a string that isn't a version counts as 0.0.0.
func Compare(a, b string) int {
	va, _ := parse(a)
	vb, _ := parse(b)
	return compare(va, vb)
}

// version is a SemVer version: major.minor.patch and its pre-release identifiers. ahead marks a build after that
// version's tag (`git describe` adds "-3-gabc1234" or "-dirty"): newer than the tag, older than the next release.
type version struct {
	core  [3]int
	pre   []string
	ahead bool
}

var describeSuffix = regexp.MustCompile(`(-[0-9]+-g[0-9a-f]+)?(-dirty)?$`)

func parse(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	base := describeSuffix.ReplaceAllString(s, "")
	ahead := base != s
	s = base
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.core[i] = n
	}
	if hasPre {
		v.pre = strings.Split(pre, ".")
	}
	v.ahead = ahead
	return v, true
}

// compare orders versions by SemVer precedence: the core first, then a pre-release below its release, then the
// pre-release identifiers one by one (numbers numerically and below words, which compare as text; more is higher).
func compare(a, b version) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			return cmpInt(a.core[i], b.core[i])
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return cmpBool(a.ahead, b.ahead)
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, xerr := strconv.Atoi(a.pre[i])
		y, yerr := strconv.Atoi(b.pre[i])
		switch {
		case xerr == nil && yerr == nil:
			if x != y {
				return cmpInt(x, y)
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(a.pre[i], b.pre[i]); c != 0 {
				return c
			}
		}
	}
	if len(a.pre) != len(b.pre) {
		return cmpInt(len(a.pre), len(b.pre))
	}
	return cmpBool(a.ahead, b.ahead)
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ErrNoDaemon means the daemon's page didn't answer, so there is no running daemon to update.
var ErrNoDaemon = errors.New("the daemon isn't running")

// Request asks the running daemon to install the newest release, as the page's button does, and returns what it
// said. ErrNoDaemon means nothing answered at cfg's address.
func Request(cfg config.Config, st *store.Store) (string, error) {
	token, err := st.CSRFToken()
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(cfg.WebURL()+"/update", url.Values{"csrf": {token}})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}
	defer resp.Body.Close()
	if loc, err := resp.Location(); err == nil {
		if msg := loc.Query().Get("msg"); msg != "" {
			return msg, nil
		}
		return "Update started.", nil // the page follows it from here, without a message
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
}
