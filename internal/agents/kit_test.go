package agents

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func names(rs []Reviewer) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// With nothing configured, the reviewers and prompts are the built-in ones, and the prompts say nothing about notes.
func TestBuiltinKit(t *testing.T) {
	k, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(k.Reviewers); !slices.Equal(got, []string{"logic", "security", "integration", "infra"}) {
		t.Errorf("reviewers = %v", got)
	}
	logic, _ := k.Reviewer("logic")
	infra, _ := k.Reviewer("infra")
	if !logic.Docs || logic.Paths != nil || !strings.Contains(logic.Focus, "off-by-one") {
		t.Errorf("logic = %+v", logic)
	}
	if !infra.Runs([]string{"terragrunt/prod/iam.hcl"}, false) || infra.Runs([]string{"cmd/main.go"}, false) {
		t.Error("infra should run on infrastructure files only")
	}
	for _, name := range Prompts {
		if k.Sources[name] != "built-in" {
			t.Errorf("prompt %s from %q", name, k.Sources[name])
		}
	}
	prompt, err := k.prompt("review", promptData{Input: Input{Repo: "o/r", Owner: "octocat"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "Notes from") {
		t.Error("without context.md the prompt must not mention notes")
	}
}

// The config directory changes a built-in reviewer field by field, switches one off, adds one, replaces a prompt,
// and gives every prompt the context.
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "agents/security.md", "---\nmodel: opus # keep it strong\n---\n")
	write(t, dir, "agents/integration.md", "---\nenabled: false\n---\n")
	write(t, dir, "agents/terraform-cost.md", `---
area: cloud cost
paths: '\.tf$'
---
Resources that cost more than they need to.
`)
	write(t, dir, "prompts/verify.md", "Custom verifier for {{.Repo}}. {{.Findings}}")
	write(t, dir, "context.md", "\nOrg-wide ADRs live in acme/architecture.\n")
	k, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(k.Reviewers); !slices.Equal(got, []string{"logic", "security", "infra", "terraform-cost"}) {
		t.Errorf("reviewers = %v", got)
	}
	if got := names(k.Disabled); !slices.Equal(got, []string{"integration"}) {
		t.Errorf("disabled = %v", got)
	}
	sec, _ := k.Reviewer("security")
	if sec.Model != "opus" || sec.Area != "security" || !strings.Contains(sec.Focus, "SSRF") ||
		sec.Source != filepath.Join(dir, "agents/security.md") {
		t.Errorf("security should keep its area and focus and pin opus: %+v", sec)
	}
	cost, _ := k.Reviewer("terraform-cost")
	if !cost.Runs([]string{"main.tf"}, false) || cost.Runs([]string{"main.go"}, false) || cost.Runs([]string{"main.tf"}, true) {
		t.Errorf("terraform-cost runs on the wrong PRs: %+v", cost)
	}
	if k.Sources["verify"] != filepath.Join(dir, "prompts/verify.md") || k.Sources["review"] != "built-in" {
		t.Errorf("sources = %v", k.Sources)
	}
	verify, _ := k.prompt("verify", promptData{Input: Input{Repo: "acme/api"}, Findings: "[]"})
	if verify != "Custom verifier for acme/api. []" {
		t.Errorf("verify prompt = %q", verify)
	}
	for _, name := range []string{"review", "design", "followup"} {
		p, err := k.prompt(name, promptData{Input: Input{Repo: "acme/api", Owner: "octocat"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p, "Notes from @octocat about their organisation") ||
			!strings.Contains(p, "Org-wide ADRs live in acme/architecture.\n\nPR acme/api#") {
			t.Errorf("%s prompt doesn't carry the context before the PR:\n%s", name, p)
		}
	}
}

// A file that can't be used stops the review with a message naming the file, instead of reviewing without it.
func TestLoadErrors(t *testing.T) {
	cases := map[string]struct{ file, text, want string }{
		"bad regexp":        {"agents/x.md", "---\narea: a\npaths: (\n---\nbody", "paths:"},
		"unknown key":       {"agents/x.md", "---\narea: a\nwhen: always\n---\nbody", `unknown key "when"`},
		"not key: value":    {"agents/x.md", "---\narea a\n---\nbody", "want key: value"},
		"no closing ---":    {"agents/x.md", "---\narea: a\nbody", "no closing ---"},
		"new without area":  {"agents/x.md", "just a body", "needs an area"},
		"reserved name":     {"agents/verifier.md", "---\narea: a\n---\nbody", "not design, followup, verifier"},
		"bad name":          {"agents/My Reviewer.md", "---\narea: a\n---\nbody", "lowercase letters"},
		"bad boolean":       {"agents/x.md", "---\narea: a\ndocs: maybe\n---\nbody", "docs: want true or false"},
		"unknown field":     {"prompts/review.md", "{{.Nope}}", "prompt review"},
		"template mistake":  {"prompts/design.md", "{{if .Repo}}", "prompt design"},
		"empty focus added": {"agents/x.md", "---\narea: a\n---\n", "needs an area"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		write(t, dir, c.file, c.text)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

// Export writes the built-ins to copy from, never overwrites, and what it writes loads back as the same reviewers.
func TestExport(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "prompts/review.md", "mine")
	wrote, err := Export(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 9 || slices.Contains(wrote, filepath.Join(dir, "prompts/review.md")) {
		t.Errorf("wrote %v; want everything but the existing prompts/review.md", wrote)
	}
	if again, _ := Export(dir); len(again) != 0 {
		t.Errorf("a second export overwrote %v", again)
	}
	_ = os.Remove(filepath.Join(dir, "prompts/review.md"))
	exported, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range Builtin().Reviewers {
		e := exported.Reviewers[i]
		if e.Name != r.Name || e.Area != r.Area || e.Focus != r.Focus || e.Docs != r.Docs || (e.Paths == nil) != (r.Paths == nil) {
			t.Errorf("exported %s differs: %+v", r.Name, e)
		}
	}
	if !strings.Contains(exported.Context, "Org-wide decisions") {
		t.Errorf("context = %q", exported.Context)
	}
}
