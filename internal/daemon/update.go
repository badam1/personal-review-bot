package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/update"
	"github.com/badam1/personal-review-bot/internal/version"
)

// ErrRestart is what Run returns once a new binary is in place and no review is running: the caller restarts into it.
var ErrRestart = errors.New("restart into the updated binary")

// checkEvery is how often the daemon looks for a new release (one GitHub API call).
const checkEvery = time.Hour

// updater looks for new releases, installs one when you ask (from the page, the TUI or the CLI), and then has the
// daemon stop taking reviews and restart once the ones it is running have finished.
type updater struct {
	d       pipeline.Deps
	src     update.Source
	verify  update.Verifier
	exe     string        // the running binary, which an update replaces
	current func() string // the running version
	busy    atomic.Bool   // an update is installing or waiting to restart
	drain   chan struct{} // closed once the new binary is in place
	running atomic.Int64  // reviews this process has claimed and not finished
}

func newUpdater(d pipeline.Deps, src update.Source, exe string) *updater {
	return &updater{d: d, src: src, verify: update.Attested, exe: exe, current: version.String, drain: make(chan struct{})}
}

// check looks for a newer release and records what it found for the page and the TUI.
func (u *updater) check() (update.Release, bool, error) {
	c := update.Check{Checked: time.Now()}
	releases, err := u.src.Releases(u.d.Cfg.UpdateRepo)
	if err != nil {
		c.Err = err.Error()
		update.Save(u.d.Store, c)
		return update.Release{}, false, err
	}
	rel, ok := update.Newest(releases, u.current(), runtime.GOOS, runtime.GOARCH)
	if ok {
		c.Latest = &rel
	}
	update.Save(u.d.Store, c)
	return rel, ok, nil
}

// loop checks at start and then every checkEvery, until ctx ends.
func (u *updater) loop(ctx context.Context) {
	for {
		if rel, ok, err := u.check(); err != nil {
			u.d.Log("update check: %v", err)
		} else if ok {
			u.d.Log("update available: %s (running %s)", rel.Version, u.current())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(checkEvery):
		}
	}
}

// CheckNow looks for a newer release right away and says what it found.
func (u *updater) CheckNow() (string, error) {
	rel, ok, err := u.check()
	switch {
	case err != nil:
		return "", fmt.Errorf("couldn't look for releases: %w", err)
	case !ok:
		return fmt.Sprintf("Up to date (%s).", u.current()), nil
	}
	return fmt.Sprintf("%s is available.", rel.Version), nil
}

// Start installs the newest release in the background and says what it is doing. Once the binary is in place the
// daemon stops taking reviews and restarts when the running ones have finished.
func (u *updater) Start() (string, error) {
	if !u.busy.CompareAndSwap(false, true) {
		return "", fmt.Errorf("an update is already under way: %s", update.State(u.d.Store))
	}
	rel, ok, err := u.check()
	switch {
	case err != nil:
		u.busy.Store(false)
		return "", fmt.Errorf("couldn't look for releases: %w", err)
	case !ok:
		u.busy.Store(false)
		return fmt.Sprintf("Already up to date (%s).", u.current()), nil
	}
	update.SetState(u.d.Store, "installing "+rel.Version)
	go func() {
		if err := update.Install(context.Background(), u.src, u.d.Cfg.UpdateRepo, rel, u.exe, u.verify); err != nil {
			update.SetState(u.d.Store, fmt.Sprintf("updating to %s failed: %v", rel.Version, err))
			u.d.Log("update to %s failed: %v", rel.Version, err)
			u.busy.Store(false)
			return
		}
		update.SetState(u.d.Store, fmt.Sprintf("%s installed: restarting once the running reviews finish", rel.Version))
		u.d.Log("%s installed in %s; restarting once the running reviews finish", rel.Version, u.exe)
		close(u.drain)
	}()
	return fmt.Sprintf("Updating to %s: it restarts once the running reviews finish.", rel.Version), nil
}

// draining reports whether workers should stop taking reviews.
func (u *updater) draining() bool {
	select {
	case <-u.drain:
		return true
	default:
		return false
	}
}

// waitIdle returns once no review is running in this process, or ctx ends.
func (u *updater) waitIdle(ctx context.Context) error {
	for u.running.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil
}
