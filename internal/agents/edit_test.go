package agents

import (
	"os"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A built-in reviewer's file says only what you changed, so the rest keeps up with new versions; changing nothing
// leaves no file.
func TestSaveBuiltinReviewer(t *testing.T) {
	dir := t.TempDir()
	sec, _ := BuiltinReviewer("security")
	e := sec.Edit(true)
	e.Model = "opus"
	path, err := SaveReviewer(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "---\nmodel: opus\n---\n" {
		t.Errorf("wrote %q", got)
	}
	k, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := k.Reviewer("security"); r.Model != "opus" || r.Focus != sec.Focus || r.Source != path {
		t.Errorf("loaded %+v", r)
	}

	e.Model, e.Enabled = "", false
	if _, err := SaveReviewer(dir, e); err != nil {
		t.Fatal(err)
	}
	if k, _ := Load(dir); len(k.Disabled) != 1 || k.Disabled[0].Name != "security" {
		t.Errorf("switched off: %v, still on: %v", names(k.Disabled), names(k.Reviewers))
	}

	e.Enabled = true
	if path, err := SaveReviewer(dir, e); err != nil || path != "" {
		t.Fatalf("as shipped: %q %v", path, err)
	}
	if _, err := os.Stat(ReviewerPath(dir, "security")); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
}

// A reviewer you add is written whole, and reads back as it was saved, quotes and comment marks included.
func TestSaveNewReviewer(t *testing.T) {
	dir := t.TempDir()
	if err := NewReviewerName(dir, "cost"); err != nil {
		t.Fatal(err)
	}
	e := ReviewerEdit{Name: "cost", Area: `cloud cost #1 "money"`, Paths: `\.(tf|tfvars)$`, Focus: "Instance sizes.\r\nNAT gateways.\r\n",
		Docs: false, Enabled: true}
	path, err := SaveReviewer(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	want := "---\narea: \"cloud cost #1 \"money\"\"\npaths: \\.(tf|tfvars)$\n---\nInstance sizes.\nNAT gateways.\n"
	if got := read(t, path); got != want {
		t.Errorf("wrote %q\nwant %q", got, want)
	}
	k, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := k.Reviewer("cost")
	if !ok || r.Area != e.Area || r.Paths.String() != e.Paths || r.Focus != "Instance sizes.\nNAT gateways." {
		t.Errorf("loaded %+v", r)
	}
	for _, name := range []string{"cost", "logic", "verifier", "Cost", "a b"} {
		if NewReviewerName(dir, name) == nil {
			t.Errorf("%q was accepted as a new name", name)
		}
	}
}

// What wouldn't load isn't written.
func TestSaveReviewerErrors(t *testing.T) {
	dir := t.TempDir()
	for _, e := range []ReviewerEdit{
		{Name: "cost", Area: "cost", Paths: "(", Focus: "x", Enabled: true},
		{Name: "cost", Area: "", Focus: "x", Enabled: true},
		{Name: "cost", Area: "a\nmodel: opus", Focus: "x", Enabled: true},
		{Name: "../x", Area: "a", Focus: "x", Enabled: true},
	} {
		if _, err := SaveReviewer(dir, e); err == nil {
			t.Errorf("saved %+v", e)
		}
	}
	if _, err := os.Stat(ReviewerPath(dir, "cost")); !os.IsNotExist(err) {
		t.Errorf("a file was left: %v", err)
	}
}

func TestSavePrompt(t *testing.T) {
	dir := t.TempDir()
	if _, err := SavePrompt(dir, "summary", "Say {{.Nope}}"); err == nil {
		t.Error("a template naming a field that doesn't exist was saved")
	}
	if _, err := SavePrompt(dir, "nope", "x"); err == nil {
		t.Error("a prompt that doesn't exist was saved")
	}
	path, err := SavePrompt(dir, "summary", "Summarise {{.Title}} in two sentences.")
	if err != nil {
		t.Fatal(err)
	}
	if text, custom, _ := PromptText(dir, "summary"); !custom || text != "Summarise {{.Title}} in two sentences.\n" || read(t, path) != text {
		t.Errorf("read back %q (custom %t)", text, custom)
	}
	builtin, _ := BuiltinPrompt("summary")
	if path, err := SavePrompt(dir, "summary", strings.ReplaceAll(builtin, "\n", "\r\n")); err != nil || path != "" {
		t.Fatalf("the built-in text: %q %v", path, err)
	}
	if _, custom, _ := PromptText(dir, "summary"); custom {
		t.Error("saving the built-in text left a file")
	}
}

func TestSaveContext(t *testing.T) {
	dir := t.TempDir()
	if err := SaveContext(dir, "ADRs live in acme/architecture.\r\n"); err != nil {
		t.Fatal(err)
	}
	if k, _ := Load(dir); k.Context != "ADRs live in acme/architecture." {
		t.Errorf("context %q", k.Context)
	}
	if err := SaveContext(dir, "  \n"); err != nil {
		t.Fatal(err)
	}
	if k, _ := Load(dir); k.Context != "" || k.Sources["context"] != "" {
		t.Errorf("emptied, still %q from %q", k.Context, k.Sources["context"])
	}
}
