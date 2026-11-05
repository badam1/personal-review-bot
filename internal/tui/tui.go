// Package tui is the terminal view: what's running right now and the last 10 reviews.
package tui

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
	"github.com/badam1/personal-review-bot/internal/update"
	"github.com/badam1/personal-review-bot/internal/version"
	"github.com/badam1/personal-review-bot/internal/web"
)

var (
	// Catppuccin Mocha
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#cba6f7"))
	dim        = lipgloss.NewStyle().Foreground(lipgloss.Color("#7f849c"))
	headStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#a6adc8"))
	selStyle   = lipgloss.NewStyle().Background(lipgloss.Color("#313244")).Foreground(lipgloss.Color("#cdd6f4"))
	runStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#89b4fa"))
	needStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#f9e2af"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#a6e3a1"))
	badStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#f38ba8"))
)

type snapshot struct {
	active, recent []store.Review
	lastPoll       time.Time
	usage          agents.Usage
	hasUsage       bool
	spentToday     float64        // on reviews since midnight, dry runs left out (as on the stats page)
	spentWeek      float64        // in the last 7 days
	update         update.Release // a newer release ("" Version: none)
	updateState    string         // what an update under way is doing
	daemonVersion  string         // what the daemon last started as
	updated        *update.Done   // an update that finished within the hour
	err            error
}

func load(st *store.Store) snapshot {
	var s snapshot
	s.active, s.err = st.Active()
	if s.err == nil {
		s.recent, s.err = st.Recent(10)
	}
	s.lastPoll, _ = time.Parse(time.RFC3339, st.Meta("last_poll"))
	s.usage, s.hasUsage = pipeline.LoadUsage(st)
	now := time.Now()
	if t, err := st.Totals(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())); err == nil {
		s.spentToday = t.CostUSD
	}
	if t, err := st.Totals(now.AddDate(0, 0, -7)); err == nil {
		s.spentWeek = t.CostUSD
	}
	s.update, _ = update.Available(update.Load(st), version.String())
	s.updateState = update.State(st)
	s.daemonVersion = update.DaemonVersion(st)
	if d, ok := update.LastDone(st, time.Now()); ok {
		s.updated = &d
	}
	return s
}

type model struct {
	cfg    config.Config
	st     *store.Store
	snap   snapshot
	cursor int
	msg    string
	width  int
	height int
}

type tickMsg struct{}

// updateMsg is what the daemon said to an update request.
type updateMsg string

func tick() tea.Cmd { return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m model) Init() tea.Cmd { return tick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.snap = load(m.st)
		return m, tick()
	case updateMsg:
		m.msg = string(msg)
		m.snap = load(m.st)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		n := len(m.snap.recent)
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < n-1 {
				m.cursor++
			}
		case "enter":
			if r, ok := m.selected(); ok {
				m.msg = openURL(fmt.Sprintf("%s/r/%d", m.cfg.WebURL(), r.ID), "the review page")
			}
		case "o":
			if r, ok := m.selected(); ok {
				m.msg = openURL(r.URL, "the PR on GitHub")
			}
		case "u": // the daemon looks for the newest release itself, so this works right after a release too
			m.msg = "asking the daemon for the newest release…"
			return m, func() tea.Msg { return updateMsg(requestUpdate(m.cfg, m.st)) }
		case "r", "d":
			if r, ok := m.selected(); ok {
				m.msg = queue(m.st, r, msg.String() == "d")
				m.snap = load(m.st)
			}
		}
	}
	return m, nil
}

func (m model) selected() (store.Review, bool) {
	if m.cursor < len(m.snap.recent) {
		return m.snap.recent[m.cursor], true
	}
	return store.Review{}, false
}

// requestUpdate asks the daemon to install the newest release and says what came of it.
func requestUpdate(cfg config.Config, st *store.Store) string {
	msg, err := update.Request(cfg, st)
	switch {
	case errors.Is(err, update.ErrNoDaemon):
		return "the daemon isn't running: run `personal-review-bot update` to update the binary"
	case err != nil:
		return "update: " + err.Error()
	}
	return msg
}

// queue asks the daemon to (re)review the selected PR; deep reviews it as if it were large (Opus, the design review).
func queue(st *store.Store, r store.Review, deep bool) string {
	if r.Status == store.Queued || r.Status == store.Running {
		return "already in progress"
	}
	if _, err := st.Insert(&store.Review{Repo: r.Repo, Number: r.Number, URL: r.URL, Title: r.Title, Author: r.Author,
		HeadSHA: r.HeadSHA, Status: store.Queued, Source: "manual", Deep: deep}); err != nil {
		return "queue failed: " + err.Error()
	}
	if deep {
		return fmt.Sprintf("queued a deep review of %s#%d", r.Repo, r.Number)
	}
	return fmt.Sprintf("queued %s#%d", r.Repo, r.Number)
}

func (m model) View() string { return render(m.cfg, m.snap, m.cursor, m.msg, true, m.width, m.height) }

func money(usd float64) string { return fmt.Sprintf("$%.2f", usd) }

// cost is a review's price for its column: "–" when it spent nothing (skipped, or failed before any Claude run).
func cost(usd float64) string {
	if usd < 0.005 {
		return "–"
	}
	return money(usd)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// usageLine shows how full the Claude plan's limits are, and whether reviews are saving because of it.
func usageLine(cfg config.Config, u agents.Usage, now time.Time) string {
	used, _ := u.Peak(now)
	five, week := u.At(now)
	line := fmt.Sprintf("plan usage: 5h %.0f%% · week %.0f%% (read %s ago)", five*100, week*100, now.Sub(u.Seen).Round(time.Minute))
	switch {
	case used*100 >= float64(cfg.MinAt):
		return badStyle.Render(line+" · cheapest models, low effort, no design review") + "\n"
	case used*100 >= float64(cfg.SaveAt):
		return needStyle.Render(line+" · cheaper models, medium effort") + "\n"
	}
	return dim.Render(line) + "\n"
}

// updateLine says that a newer release is out, or what an update under way is doing ("" when neither).
func updateLine(s snapshot, interactive bool) string {
	var lines []string
	busy := update.Busy(s.updateState)
	switch {
	case busy:
		lines = append(lines, needStyle.Render("updating: "+s.updateState))
	case s.updateState != "":
		lines = append(lines, badStyle.Render("update: "+s.updateState))
	}
	if d := s.updated; d != nil && d.To == s.daemonVersion && !busy {
		lines = append(lines, okStyle.Render(fmt.Sprintf("✓ updated to %s (from %s), %s ago", d.To, d.From,
			time.Since(d.At).Round(time.Minute))))
	}
	if s.update.Version != "" && !busy {
		how := "u to update"
		if !interactive {
			how = "personal-review-bot update"
		}
		lines = append(lines, needStyle.Render(fmt.Sprintf("%s is available (running %s) · %s", s.update.Version,
			version.String(), how)))
	}
	// The TUI is its own process: after the daemon updates, this one still runs the old binary.
	if interactive && s.daemonVersion != "" && s.daemonVersion != version.String() && !busy {
		lines = append(lines, needStyle.Render(fmt.Sprintf("the daemon runs %s, this TUI %s: restart it (q, then personal-review-bot)",
			s.daemonVersion, version.String())))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// header is the top of the screen: the title and version, whether the daemon runs, the posting rule and the page, the
// plan's usage and any update, with the logo beside it when the terminal has room for it.
func header(cfg config.Config, s snapshot, interactive bool, width, height int) string {
	withLogo := interactive && width >= 64 && height >= 26
	title := "🤖 personal-review-bot"
	if withLogo {
		title = "personal-review-bot"
	}
	daemon := badStyle.Render("● daemon not running")
	if !s.lastPoll.IsZero() && time.Since(s.lastPoll) < 2*cfg.PollInterval+30*time.Second {
		daemon = okStyle.Render(fmt.Sprintf("● watching (checked %s ago)", time.Since(s.lastPoll).Round(time.Second)))
	}
	rule := "auto-post off"
	if sev := cfg.AutoPostSeverities(); sev != "" {
		rule = fmt.Sprintf("auto-post ≥ %d%% · %s", cfg.Threshold, sev)
	}
	lines := []string{titleStyle.Render(title) + " " + dim.Render(version.String()), daemon,
		dim.Render(rule + " · " + cfg.WebURL())}
	lines = append(lines, dim.Render(fmt.Sprintf("spend: today %s · 7 days %s", money(s.spentToday), money(s.spentWeek))))
	if s.hasUsage {
		lines = append(lines, strings.TrimSuffix(usageLine(cfg, s.usage, time.Now()), "\n"))
	}
	if u := updateLine(s, interactive); u != "" {
		lines = append(lines, strings.TrimSuffix(u, "\n"))
	}
	text := strings.Join(lines, "\n")
	if !withLogo {
		return text + "\n"
	}
	return lipgloss.JoinHorizontal(lipgloss.Center, logo(), "   ", text) + "\n"
}

func render(cfg config.Config, s snapshot, cursor int, msg string, interactive bool, width, height int) string {
	var b strings.Builder
	b.WriteString(header(cfg, s, interactive, width, height))
	b.WriteString("\n")
	if s.err != nil {
		fmt.Fprintf(&b, "%s\n\n", badStyle.Render("database: "+s.err.Error()))
	}

	b.WriteString(headStyle.Render("IN PROGRESS") + "\n")
	if len(s.active) == 0 {
		b.WriteString(dim.Render("  nothing running") + "\n")
	}
	for _, r := range s.active {
		elapsed := ""
		if !r.StartedAt.IsZero() && r.Status == store.Running {
			elapsed = " · " + time.Since(r.StartedAt).Round(time.Second).String()
		}
		if it := web.IterationLabel(r); it != "" {
			elapsed += " · " + it
		}
		if r.CostUSD > 0 {
			elapsed += " · " + money(r.CostUSD) + " so far"
		}
		fmt.Fprintf(&b, "  %s %s %s\n", runStyle.Render(web.StatusLabel(r)),
			fmt.Sprintf("%s#%d %s", short(r.Repo), r.Number, trunc(r.Title, 50)), dim.Render(r.Tier+elapsed))
	}

	b.WriteString("\n" + headStyle.Render("LAST 10") + "\n")
	titleW := 42
	if width > 148 {
		titleW = width - 106
	}
	for i, r := range s.recent {
		status := web.StatusLabel(r)
		switch r.Status {
		case store.AwaitingYou:
			status = needStyle.Render(status)
		case store.Failed:
			status = badStyle.Render(status)
		case store.Running, store.Queued:
			status = runStyle.Render(trunc(status, 24))
		}
		dry := "  "
		switch {
		case r.DryRun:
			dry = "🧪"
		case r.Deep:
			dry = "🔬"
		case r.Source == "re-request" || r.Source == "new commits":
			dry = "🔁"
		}
		pr := fmt.Sprintf("%s#%d", short(r.Repo), r.Number)
		if r.Reran() {
			pr += " · " + web.Ordinal(r.Iteration)
		}
		line := fmt.Sprintf("%-10s %-26s %-*s %-16s %-14s %s %7s %s",
			agoShort(r.CreatedAt), trunc(pr, 26), titleW, trunc(r.Title, titleW),
			trunc("@"+r.Author, 16), status, dry, cost(r.CostUSD), dim.Render(trunc(r.Decision, 44)))
		if interactive && i == cursor {
			line = selStyle.Render(line)
		}
		b.WriteString(" " + line + "\n")
	}
	if len(s.recent) == 0 {
		b.WriteString(dim.Render("  no reviews yet") + "\n")
	}
	if interactive {
		b.WriteString("\n" + dim.Render("↑/↓ select · enter review page · o open PR · r review again · d deep review · u update · q quit"))
		b.WriteString("\n" + dim.Render(fmt.Sprintf("personal-review-bot %s · bugs and ideas: github.com/%s/issues",
			version.String(), cfg.UpdateRepo)))
		if msg != "" {
			b.WriteString("   " + okStyle.Render(msg))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func short(repo string) string {
	_, name, ok := strings.Cut(repo, "/")
	if !ok {
		return repo
	}
	return name
}

func agoShort(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if time.Since(t) < 24*time.Hour {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Jan 02")
}

// Run starts the interactive terminal UI.
func Run(cfg config.Config) error {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = tea.NewProgram(model{cfg: cfg, st: st, snap: load(st)}, tea.WithAltScreen()).Run()
	return err
}

// PrintStatus prints the same view once (for scripts, and for `personal-review-bot status`).
func PrintStatus(cfg config.Config, w io.Writer) error {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = io.WriteString(w, render(cfg, load(st), -1, "", false, 160, 0))
	return err
}

// opener is the command that opens a URL in the browser: open on macOS, xdg-open on Linux and the BSDs.
func opener(goos string) (string, bool) {
	switch goos {
	case "darwin":
		return "open", true
	case "windows", "plan9":
		return "", false
	}
	return "xdg-open", true
}

// openURL opens url in the browser and says so, or, when it can't, gives the URL to open by hand.
func openURL(url, what string) string {
	if name, ok := opener(runtime.GOOS); ok {
		cmd := exec.Command(name, url)
		if cmd.Start() == nil {
			go func() { _ = cmd.Wait() }() // reap it, so it doesn't linger until the TUI exits
			return "opened " + what
		}
	}
	return "couldn't open a browser: " + url
}
