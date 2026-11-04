package triage

import (
	"slices"
	"testing"

	"github.com/badam1/personal-review-bot/internal/gh"
)

func files(paths ...string) []gh.File {
	var fs []gh.File
	for _, p := range paths {
		fs = append(fs, gh.File{Path: p})
	}
	return fs
}

func TestMake(t *testing.T) {
	cases := []struct {
		name          string
		lines         int
		files         []gh.File
		tier, model   string
		verifier      string
		infra, single bool
	}{
		{"docs only", 400, files("README.md", "docs/adr/003.md"), "small", "haiku", "sonnet", false, true},
		{"small code", 40, files("cmd/main.go"), "small", "haiku", "sonnet", false, false},
		{"medium code", 300, files("internal/api/handler.go"), "medium", "sonnet", "opus", false, false},
		{"small but risky", 30, files("terragrunt/prod/iam/policy.hcl"), "medium", "sonnet", "opus", true, false},
		{"risky and bigger", 200, files("flux/clusters/gke-prod/app.yaml"), "large", "opus", "opus", true, false},
		{"huge", 2000, files("web/src/app.tsx"), "large", "opus", "opus", false, false},
	}
	for _, c := range cases {
		p := Make(gh.PR{Additions: c.lines}, c.files)
		if p.Tier != c.tier || p.AgentModel != c.model || p.VerifierModel != c.verifier {
			t.Errorf("%s: got %s %s→%s, want %s %s→%s", c.name, p.Tier, p.AgentModel, p.VerifierModel, c.tier, c.model, c.verifier)
		}
		if got := slices.Contains(p.Agents, "infra"); got != c.infra {
			t.Errorf("%s: infra reviewer = %v, want %v", c.name, got, c.infra)
		}
		if c.single != (len(p.Agents) == 1) {
			t.Errorf("%s: agents %v", c.name, p.Agents)
		}
		if p.VerifierModel == "haiku" {
			t.Errorf("%s: verifier must never be haiku", c.name)
		}
	}
}

// The design is reviewed on medium and large PRs, and on any PR that touches a recorded decision.
func TestDesign(t *testing.T) {
	cases := []struct {
		name  string
		lines int
		files []gh.File
		want  bool
	}{
		{"small code", 40, files("cmd/main.go"), false},
		{"medium code", 300, files("internal/api/handler.go"), true},
		{"large code", 2000, files("web/src/app.tsx"), true},
		{"small but risky", 30, files("terragrunt/prod/iam/policy.hcl"), true},
		{"docs only", 400, files("README.md", "docs/guide.md"), false},
		{"new infrastructure ADR", 60, files("ADR/ADR-014-something.md", "ADR/index.md"), true},
		{"docs/adr", 20, files("docs/adr/0003-use-x.md"), true},
		{"decisions folder", 20, files("decisions/kafka.md"), true},
		{"word adr in a code path", 20, files("internal/address/adress.go"), false},
	}
	for _, c := range cases {
		if got := Make(gh.PR{Additions: c.lines}, c.files).Design; got != c.want {
			t.Errorf("%s: design = %v, want %v", c.name, got, c.want)
		}
	}
}

// Only large PRs are worth Fable's price for the design review.
func TestDesignModel(t *testing.T) {
	cases := []struct {
		name  string
		lines int
		files []gh.File
		want  string
	}{
		{"medium code", 300, files("internal/api/handler.go"), "opus"},
		{"small ADR", 60, files("ADR/ADR-014-something.md"), "opus"},
		{"risky and bigger", 200, files("flux/clusters/gke-prod/app.yaml"), "fable"},
		{"huge", 2000, files("web/src/app.tsx"), "fable"},
	}
	for _, c := range cases {
		if got := Make(gh.PR{Additions: c.lines}, c.files).DesignModel; got != c.want {
			t.Errorf("%s: design model = %s, want %s", c.name, got, c.want)
		}
	}
}

// As the plan's limits fill up, reviews get cheaper; the verifier never drops below Sonnet.
func TestWithin(t *testing.T) {
	large := Make(gh.PR{Additions: 2000}, files("web/src/app.tsx"))   // opus → opus, design on fable
	medium := Make(gh.PR{Additions: 300}, files("internal/api/x.go")) // sonnet → opus, design on opus
	small := Make(gh.PR{Additions: 40}, files("cmd/main.go"))         // haiku → sonnet

	if p := large.Within(0.74, "week", 0.75, 0.90); p.Models() != "opus → opus" || p.Effort != "" || p.Saving != "" {
		t.Errorf("below the save line nothing changes: %s, effort %q", p.Models(), p.Effort)
	}
	p := large.Within(0.80, "week", 0.75, 0.90)
	if p.Models() != "sonnet → sonnet · saving: week 80%" || p.DesignModel != "opus" || p.Effort != "medium" || !p.Design {
		t.Errorf("saving, large: %s, design %s (%v), effort %q", p.Models(), p.DesignModel, p.Design, p.Effort)
	}
	if p := medium.Within(0.80, "5h", 0.75, 0.90); p.Models() != "haiku → sonnet · saving: 5h 80%" || p.DesignModel != "sonnet" {
		t.Errorf("saving, medium: %s, design %s", p.Models(), p.DesignModel)
	}
	if p := small.Within(0.80, "week", 0.75, 0.90); p.VerifierModel != "sonnet" {
		t.Errorf("the verifier must never drop below sonnet, got %s", p.VerifierModel)
	}
	p = large.Within(0.95, "week", 0.75, 0.90)
	if p.Models() != "haiku → sonnet · saving: week 95%" || p.Effort != "low" || p.Design {
		t.Errorf("near the limit: %s, effort %q, design %v", p.Models(), p.Effort, p.Design)
	}
}

// The follow-up check runs on Sonnet, and is left out only near the usage limit.
func TestFollowupPlan(t *testing.T) {
	p := Make(gh.PR{Additions: 300}, files("internal/api/x.go"))
	if !p.Followup || p.FollowupModel != "sonnet" {
		t.Errorf("plan = %+v", p)
	}
	if !p.Within(0.80, "week", 0.75, 0.90).Followup || p.Within(0.95, "week", 0.75, 0.90).Followup {
		t.Error("the follow-up stays when saving and goes near the limit")
	}
}
