package agents

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"
)

// Reviewer is one code reviewer: what it looks for and when it runs. The built-in ones are reviewers/*.md; in the
// config directory, agents/<name>.md changes one (the fields it sets, and its focus if the body isn't empty), switches
// it off (enabled: false), or adds a new one.
type Reviewer struct {
	Name   string         // the file name without .md
	Area   string         // e.g. "logic and correctness"
	Focus  string         // what to look for: the file's body
	Paths  *regexp.Regexp // runs only when a changed file matches (nil = on every PR)
	Docs   bool           // also runs on PRs that only change docs
	Model  string         // pins the model ("" = the PR tier's reviewer model)
	Source string         // "built-in", or the file that defined or last changed it
}

// Runs reports whether the reviewer looks at a PR that changes these paths.
func (r Reviewer) Runs(paths []string, docsOnly bool) bool {
	if docsOnly && !r.Docs {
		return false
	}
	return r.Paths == nil || slices.ContainsFunc(paths, r.Paths.MatchString)
}

// Kit is what a review runs with: the reviewers and the prompts, the built-in ones as changed by the config directory.
type Kit struct {
	Reviewers []Reviewer        // enabled ones: the built-ins in their usual order, then yours by name
	Disabled  []Reviewer        // reviewers switched off with enabled: false
	Context   string            // context.md: what every prompt should know about your organisation
	Sources   map[string]string // where each prompt and the context came from: "built-in" or a file
	prompts   map[string]*template.Template
}

//go:embed reviewers/*.md
var builtinReviewers embed.FS

//go:embed prompts/*.md
var builtinPrompts embed.FS

// Prompts are the templates every review uses; a file of the same name in the config directory's prompts/ replaces one.
var Prompts = []string{"review", "verify", "design", "followup", "summary"}

var (
	builtinOrder = []string{"logic", "security", "integration", "infra"}
	reserved     = []string{DesignAgent, FollowupAgent, VerifierAgent, SummaryAgent} // the pipeline's own runs
	validName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Builtin is the kit with nothing changed.
func Builtin() *Kit {
	k, err := Load("")
	if err != nil {
		panic(err) // the embedded files are tested
	}
	return k
}

// Load builds the kit from the built-in reviewers and prompts and the config directory: agents/*.md, prompts/*.md
// and context.md. A missing directory or file changes nothing; a file that can't be used is an error.
func Load(dir string) (*Kit, error) {
	k := &Kit{Sources: map[string]string{}, prompts: map[string]*template.Template{}}
	defs := map[string]Reviewer{}
	for _, name := range builtinOrder {
		data, err := builtinReviewers.ReadFile("reviewers/" + name + ".md")
		if err != nil {
			return nil, err
		}
		r, _, err := parseReviewer(name, "built-in", data, Reviewer{})
		if err != nil {
			return nil, fmt.Errorf("built-in reviewer %s: %w", name, err)
		}
		defs[name] = r
	}
	var added []string
	if dir != "" {
		files, _ := filepath.Glob(filepath.Join(dir, "agents", "*.md"))
		sort.Strings(files)
		for _, path := range files {
			name := strings.TrimSuffix(filepath.Base(path), ".md")
			if !validName.MatchString(name) || slices.Contains(reserved, name) {
				return nil, fmt.Errorf("%s: a reviewer's file name must be lowercase letters, digits and dashes, and not %s",
					path, strings.Join(reserved, ", "))
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			base, known := defs[name]
			r, enabled, err := parseReviewer(name, path, data, base)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if !enabled {
				if known {
					k.Disabled = append(k.Disabled, r)
				}
				delete(defs, name)
				continue
			}
			if !known {
				added = append(added, name)
			}
			defs[name] = r
		}
	}
	for _, name := range append(slices.Clone(builtinOrder), added...) {
		if r, ok := defs[name]; ok {
			k.Reviewers = append(k.Reviewers, r)
		}
	}

	for _, name := range Prompts {
		text, err := builtinPrompts.ReadFile("prompts/" + name + ".md")
		if err != nil {
			return nil, err
		}
		source := "built-in"
		if dir != "" {
			path := filepath.Join(dir, "prompts", name+".md")
			if own, err := os.ReadFile(path); err == nil {
				text, source = own, path
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		t, err := template.New(name).Parse(string(text))
		if err == nil {
			err = t.Execute(&bytes.Buffer{}, samplePrompt) // catches fields the template names that don't exist
		}
		if err != nil {
			return nil, fmt.Errorf("prompt %s (%s): %w", name, source, err)
		}
		k.prompts[name], k.Sources[name] = t, source
	}

	k.Sources["context"] = ""
	if dir != "" {
		path := filepath.Join(dir, "context.md")
		if data, err := os.ReadFile(path); err == nil {
			k.Context, k.Sources["context"] = strings.TrimSpace(string(data)), path
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return k, nil
}

// Reviewer returns the enabled reviewer of that name.
func (k *Kit) Reviewer(name string) (Reviewer, bool) {
	i := slices.IndexFunc(k.Reviewers, func(r Reviewer) bool { return r.Name == name })
	if i < 0 {
		return Reviewer{}, false
	}
	return k.Reviewers[i], true
}

// samplePrompt has every field a prompt template can use, to check a template before a review depends on it.
var samplePrompt = promptData{Input: Input{Repo: "owner/repo", Number: 1, Title: "t", Author: "a", Body: "b", Base: "main",
	Head: "h", Diff: "d", HeadSHA: "bbb", PrevSHA: "aaa", Conversation: "c", Prior: "p", Owner: "o", Org: "owner"},
	Area: "a", Focus: "f", Findings: "[]", Accounts: "- logic: read a.go", Context: "c"}

// parseReviewer reads a reviewer file: an optional front matter block of `key: value` lines between two `---` lines,
// then the focus. It starts from base, so a file changes only what it sets. It returns whether the reviewer is enabled.
func parseReviewer(name, source string, data []byte, base Reviewer) (Reviewer, bool, error) {
	r, enabled := base, true
	r.Name, r.Source = name, source
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if lines := strings.Split(text, "\n"); strings.TrimSpace(lines[0]) == "---" {
		end := slices.IndexFunc(lines[1:], func(l string) bool { return strings.TrimSpace(l) == "---" })
		if end < 0 {
			return r, false, errors.New("the front matter has no closing ---")
		}
		front := lines[1 : end+1]
		text = strings.Join(lines[end+2:], "\n")
		for i, line := range front {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, raw, ok := strings.Cut(line, ":")
			if !ok {
				return r, false, fmt.Errorf("front matter line %d: want key: value, got %q", i+1, line)
			}
			key, value := strings.TrimSpace(key), frontValue(strings.TrimSpace(raw))
			var err error
			switch key {
			case "area":
				r.Area = value
			case "model":
				r.Model = value
			case "paths":
				r.Paths = nil
				if value != "" {
					if r.Paths, err = regexp.Compile(value); err != nil {
						return r, false, fmt.Errorf("paths: %w", err)
					}
				}
			case "docs":
				r.Docs, err = boolean(value)
			case "enabled":
				enabled, err = boolean(value)
			default:
				return r, false, fmt.Errorf("unknown key %q (want area, paths, docs, model or enabled)", key)
			}
			if err != nil {
				return r, false, fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	if focus := strings.TrimSpace(text); focus != "" {
		r.Focus = focus
	}
	if enabled && (r.Area == "" || r.Focus == "") {
		return r, false, errors.New("a reviewer needs an area (in the front matter) and what to look for (the body)")
	}
	return r, enabled, nil
}

// frontValue reads a front matter value as YAML would a plain one: a quoted value is taken whole, otherwise " #" starts
// a comment.
func frontValue(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	if before, _, ok := strings.Cut(s, " #"); ok {
		return strings.TrimSpace(before)
	}
	return s
}

func boolean(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("want true or false, got %q", s)
}

// contextExample is what `agents export` writes as context.md: a start for the notes every prompt gets.
const contextExample = `Notes about your organisation that every reviewer, the verifier, the design reviewer and the follow-up check
read before the PR. Write what they can't find in the repo itself, for example:

- Org-wide decisions (ADRs) live in my-org/architecture under adr/; read adr/index.md first. Only accepted ones bind.
- Tasks are GitHub issues in my-org/roadmap; a PR's description links its issue.
- Every service releases through the shared workflow my-org/workflows/.github/workflows/release.yml.
`

// Export writes the built-in reviewers (agents/), prompts (prompts/) and an example context.md into dir, to copy the
// ones you want to change into the config directory. It never overwrites a file; it returns the files it wrote.
func Export(dir string) ([]string, error) {
	files := map[string][]byte{"context.md": []byte(contextExample)}
	for _, name := range builtinOrder {
		data, err := builtinReviewers.ReadFile("reviewers/" + name + ".md")
		if err != nil {
			return nil, err
		}
		files[filepath.Join("agents", name+".md")] = data
	}
	for _, name := range Prompts {
		data, err := builtinPrompts.ReadFile("prompts/" + name + ".md")
		if err != nil {
			return nil, err
		}
		files[filepath.Join("prompts", name+".md")] = data
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var wrote []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, path)
	}
	return wrote, nil
}
