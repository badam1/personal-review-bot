// Package tui is the terminal view: what's running right now and the last 10 reviews.
package tui

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/badam1/personal-review-bot/internal/agents"
	"github.com/badam1/personal-review-bot/internal/config"
	"github.com/badam1/personal-review-bot/internal/pipeline"
	"github.com/badam1/personal-review-bot/internal/store"
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
	return s
}

type model struct {
	cfg    config.Config
	st     *store.Store
	snap   snapshot
	cursor int
	msg    string
	width  int
}

type tickMsg struct{}

func tick() tea.Cmd { return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m model) Init() tea.Cmd { return tick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.snap = load(m.st)
		return m, tick()
	case tea.WindowSizeMsg:
		m.width = msg.Width
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
				_ = exec.Command("open", fmt.Sprintf("%s/r/%d", m.cfg.WebURL(), r.ID)).Start()
				m.msg = "opened the review page"
			}
		case "o":
			if r, ok := m.selected(); ok {
				_ = exec.Command("open", r.URL).Start()
				m.msg = "opened the PR on GitHub"
			}
		case "r":
			if r, ok := m.selected(); ok {
				m.msg = queue(m.st, r)
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

// queue asks the daemon to (re)review the selected PR.
func queue(st *store.Store, r store.Review) string {
	if r.Status == store.Queued || r.Status == store.Running {
		return "already in progress"
	}
	if _, err := st.Insert(&store.Review{Repo: r.Repo, Number: r.Number, URL: r.URL, Title: r.Title, Author: r.Author,
		HeadSHA: r.HeadSHA, Status: store.Queued, Source: "manual"}); err != nil {
		return "queue failed: " + err.Error()
	}
	return fmt.Sprintf("queued %s#%d", r.Repo, r.Number)
}

func (m model) View() string { return render(m.cfg, m.snap, m.cursor, m.msg, true, m.width) }

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

func render(cfg config.Config, s snapshot, cursor int, msg string, interactive bool, width int) string {
	var b strings.Builder
	daemon := badStyle.Render("● daemon not running")
	if !s.lastPoll.IsZero() && time.Since(s.lastPoll) < 2*cfg.PollInterval+30*time.Second {
		daemon = okStyle.Render(fmt.Sprintf("● watching (checked %s ago)", time.Since(s.lastPoll).Round(time.Second)))
	}
	fmt.Fprintf(&b, "%s  %s  %s\n", titleStyle.Render("🤖 personal-review-bot"), daemon,
		dim.Render(fmt.Sprintf("auto-post ≥ %d%% · %s+ · %s", cfg.Threshold, cfg.AutoSeverity, cfg.WebURL())))
	if s.hasUsage {
		b.WriteString(usageLine(cfg, s.usage, time.Now()))
	}
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
		fmt.Fprintf(&b, "  %s %s %s\n", runStyle.Render(web.StatusLabel(r)),
			fmt.Sprintf("%s#%d %s", short(r.Repo), r.Number, trunc(r.Title, 50)), dim.Render(r.Tier+elapsed))
	}

	b.WriteString("\n" + headStyle.Render("LAST 10") + "\n")
	titleW := 42
	if width > 140 {
		titleW = width - 98
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
		case r.Source == "re-request" || r.Source == "new commits":
			dry = "🔁"
		}
		pr := fmt.Sprintf("%s#%d", short(r.Repo), r.Number)
		if r.Reran() {
			pr += " · " + web.Ordinal(r.Iteration)
		}
		line := fmt.Sprintf("%-10s %-26s %-*s %-16s %-14s %s %s",
			agoShort(r.CreatedAt), trunc(pr, 26), titleW, trunc(r.Title, titleW),
			trunc("@"+r.Author, 16), status, dry, dim.Render(trunc(r.Decision, 44)))
		if interactive && i == cursor {
			line = selStyle.Render(line)
		}
		b.WriteString(" " + line + "\n")
	}
	if len(s.recent) == 0 {
		b.WriteString(dim.Render("  no reviews yet") + "\n")
	}
	if interactive {
		b.WriteString("\n" + dim.Render("↑/↓ select · enter review page · o open PR · r review again · q quit"))
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
	_, err = io.WriteString(w, render(cfg, load(st), -1, "", false, 160))
	return err
}
