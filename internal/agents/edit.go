package agents

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/badam1/personal-review-bot/internal/atomicfile"
)

// The Settings page changes reviewers, prompts and context.md through these. Each writes the file Load reads, and
// checks it the way Load would before it does, so a page can't save something that stops the next review.

// IsBuiltin reports whether a reviewer is one of the built-in ones.
func IsBuiltin(name string) bool { return slices.Contains(builtinOrder, name) }

// BuiltinReviewer is a built-in reviewer as shipped, before agents/<name>.md changes it.
func BuiltinReviewer(name string) (Reviewer, bool) {
	if !IsBuiltin(name) {
		return Reviewer{}, false
	}
	data, err := builtinReviewers.ReadFile("reviewers/" + name + ".md")
	if err != nil {
		return Reviewer{}, false
	}
	r, _, err := parseReviewer(name, "built-in", data, Reviewer{})
	return r, err == nil
}

// ReviewerEdit is a reviewer as the Settings page edits it.
type ReviewerEdit struct {
	Name, Area, Paths, Model, Focus string
	Docs, Enabled                   bool
}

// Edit is r as the Settings page edits it.
func (r Reviewer) Edit(enabled bool) ReviewerEdit {
	e := ReviewerEdit{Name: r.Name, Area: r.Area, Model: r.Model, Focus: r.Focus, Docs: r.Docs, Enabled: enabled}
	if r.Paths != nil {
		e.Paths = r.Paths.String()
	}
	return e
}

// ReviewerPath is where a reviewer's file goes in the config directory.
func ReviewerPath(dir, name string) string { return filepath.Join(dir, "agents", name+".md") }

// NewReviewerName says what is wrong with a name for a reviewer you add (nil: nothing).
func NewReviewerName(dir, name string) error {
	if !validName.MatchString(name) || len(name) > 40 {
		return errors.New("a reviewer's name is lowercase letters, digits and dashes, up to 40")
	}
	if slices.Contains(reserved, name) {
		return fmt.Errorf("%s is one of the bot's own runs", name)
	}
	if _, err := os.Stat(ReviewerPath(dir, name)); IsBuiltin(name) || err == nil {
		return fmt.Errorf("there is already a reviewer called %s", name)
	}
	return nil
}

// SaveReviewer writes agents/<name>.md. For a built-in reviewer it writes only what differs from the built-in one,
// so the rest keeps the improvements new versions bring, and removes the file when nothing differs. It returns
// the file it wrote ("" when it removed it).
func SaveReviewer(dir string, e ReviewerEdit) (string, error) {
	path := ReviewerPath(dir, e.Name)
	if !validName.MatchString(e.Name) || slices.Contains(reserved, e.Name) {
		return "", fmt.Errorf("%q can't be a reviewer's name", e.Name)
	}
	if strings.ContainsAny(e.Area+e.Paths+e.Model, "\r\n") {
		return "", errors.New("the area, paths and model must each be on one line")
	}
	e.Focus = strings.TrimSpace(strings.ReplaceAll(e.Focus, "\r\n", "\n"))
	base, builtin := BuiltinReviewer(e.Name) // one you added starts from nothing
	was := base.Edit(true)
	var front []string
	field := func(key, value, before string) {
		if value != before {
			front = append(front, key+": "+frontQuote(value))
		}
	}
	field("area", e.Area, was.Area)
	field("paths", e.Paths, was.Paths)
	field("model", e.Model, was.Model)
	if e.Docs != was.Docs {
		front = append(front, fmt.Sprintf("docs: %t", e.Docs))
	}
	if !e.Enabled {
		front = append(front, "enabled: false")
	}
	body := e.Focus
	if builtin && body == was.Focus {
		body = ""
	}
	if builtin && len(front) == 0 && body == "" {
		return "", atomicfile.Remove(path)
	}
	var b strings.Builder
	if len(front) > 0 {
		b.WriteString("---\n" + strings.Join(front, "\n") + "\n---\n")
	}
	if body != "" {
		b.WriteString(body + "\n")
	}
	if _, _, err := parseReviewer(e.Name, path, []byte(b.String()), base); err != nil {
		return "", err
	}
	return path, atomicfile.Write(path, []byte(b.String()), 0o644)
}

// frontQuote writes a front matter value so that frontValue reads it back as it is.
func frontQuote(v string) string {
	if v == "" || v != strings.TrimSpace(v) || strings.Contains(v, " #") || strings.ContainsAny(v[:1], `"'`) {
		return `"` + v + `"`
	}
	return v
}

// RemoveReviewer removes agents/<name>.md: a built-in reviewer is as shipped again, and one you added is gone.
func RemoveReviewer(dir, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("%q isn't a reviewer's name", name)
	}
	return atomicfile.Remove(ReviewerPath(dir, name))
}

// PromptPath is where a prompt you customise goes in the config directory.
func PromptPath(dir, name string) string { return filepath.Join(dir, "prompts", name+".md") }

// BuiltinPrompt is a prompt as shipped.
func BuiltinPrompt(name string) (string, error) {
	if !slices.Contains(Prompts, name) {
		return "", fmt.Errorf("there is no prompt called %q", name)
	}
	data, err := builtinPrompts.ReadFile("prompts/" + name + ".md")
	return string(data), err
}

// PromptText is the prompt a review uses: yours from prompts/<name>.md, or the built-in one (custom false).
func PromptText(dir, name string) (text string, custom bool, err error) {
	builtin, err := BuiltinPrompt(name)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(PromptPath(dir, name))
	switch {
	case err == nil:
		return string(data), true, nil
	case errors.Is(err, fs.ErrNotExist):
		return builtin, false, nil
	}
	return "", false, err
}

// SavePrompt writes prompts/<name>.md once the text works as a template, or removes it when the text is the
// built-in one. It returns the file it wrote ("" when it removed it).
func SavePrompt(dir, name, text string) (string, error) {
	builtin, err := BuiltinPrompt(name)
	if err != nil {
		return "", err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	path := PromptPath(dir, name)
	if strings.TrimSpace(text) == strings.TrimSpace(builtin) {
		return "", atomicfile.Remove(path)
	}
	t, err := template.New(name).Parse(text)
	if err == nil {
		err = t.Execute(&bytes.Buffer{}, samplePrompt)
	}
	if err != nil {
		return "", fmt.Errorf("the prompt doesn't work as a template: %w", err)
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return path, atomicfile.Write(path, []byte(text), 0o644)
}

// ResetPrompt removes prompts/<name>.md, so reviews use the built-in prompt again.
func ResetPrompt(dir, name string) error {
	if _, err := BuiltinPrompt(name); err != nil {
		return err
	}
	return atomicfile.Remove(PromptPath(dir, name))
}

// SaveContext writes context.md, or removes it when text is empty.
func SaveContext(dir, text string) error {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	path := filepath.Join(dir, "context.md")
	if text == "" {
		return atomicfile.Remove(path)
	}
	return atomicfile.Write(path, []byte(text+"\n"), 0o644)
}
