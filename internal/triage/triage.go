// Package triage sizes a PR and picks the reviewers and models for it: small, low-risk PRs get fast cheap models;
// big or risky ones (IaC, IAM/auth, prod, migrations) get the strongest.
package triage

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/badam1/personal-review-bot/internal/gh"
)

type Plan struct {
	Tier          string   // small | medium | large
	AgentModel    string   // haiku | sonnet | opus
	VerifierModel string   // never weaker than sonnet: it decides what gets posted
	Agents        []string // logic, security, integration, infra
	Design        bool     // worth a design review (the pipeline runs it on first reviews and manual ones)
	DesignModel   string   // opus | fable: only large PRs are worth Fable's price
	Followup      bool     // check whether findings posted on earlier reviews were fixed (off near the usage limits)
	FollowupModel string   // a targeted check, so Sonnet
	Effort        string   // for every run; "" = the model's default
	Saving        string   // why the plan was made cheaper, e.g. "saving: week 82%"
	Reasons       []string
}

func (p Plan) Models() string {
	if p.Saving != "" {
		return fmt.Sprintf("%s → %s · %s", p.AgentModel, p.VerifierModel, p.Saving)
	}
	return fmt.Sprintf("%s → %s", p.AgentModel, p.VerifierModel)
}

var cheaper = map[string]string{"fable": "opus", "opus": "sonnet", "sonnet": "haiku", "haiku": "haiku"}

// Within fits the plan to how full the Claude plan's usage limits are: used (0-1) of the fuller window, named by
// window. From saveAt every model is a step cheaper and runs at medium effort; from minAt the reviewers run on Haiku at
// low effort and the design review and follow-up check are left out. The verifier never drops below Sonnet: it decides what gets posted.
func (p Plan) Within(used float64, window string, saveAt, minAt float64) Plan {
	switch {
	case used >= minAt:
		p.AgentModel, p.VerifierModel, p.Effort, p.Design, p.Followup = "haiku", "sonnet", "low", false, false
	case used >= saveAt:
		p.AgentModel, p.VerifierModel, p.DesignModel = cheaper[p.AgentModel], cheaper[p.VerifierModel], cheaper[p.DesignModel]
		if p.VerifierModel == "haiku" {
			p.VerifierModel = "sonnet"
		}
		p.Effort = "medium"
	default:
		return p
	}
	p.Saving = fmt.Sprintf("saving: %s %.0f%%", window, used*100)
	return p
}

var (
	docsFile  = regexp.MustCompile(`(?i)(\.(md|mdx|txt|rst|adoc)$|^docs?/|(^|/)(LICENSE|CODEOWNERS)$)`)
	infraFile = regexp.MustCompile(`(?i)(\.(tf|tfvars|hcl)$|terragrunt|(^|/)flux/|kustomization\.ya?ml$|(^|/)(charts?|helm|k8s|kubernetes|manifests)/|(^|/)Dockerfile|\.github/workflows/)`)
	adrFile   = regexp.MustCompile(`(?i)((^|/)(adrs?|decisions)/|(^|/)adr-[0-9]+[^/]*$)`)
	riskyFile = regexp.MustCompile(`(?i)(iam|auth|rbac|secret|kms|security|polic(y|ies)|firewall|network|vpc|migration|alembic|\.sql$|(^|[/_.-])prod(uction)?([/_.-]|$))`)
)

const (
	smallLines = 150
	largeLines = 800
)

func Make(pr gh.PR, files []gh.File) Plan {
	lines := pr.Additions + pr.Deletions
	docsOnly, infra, adr := len(files) > 0, false, false
	var risky []string
	for _, f := range files {
		if adrFile.MatchString(f.Path) {
			adr = true
		}
		if !docsFile.MatchString(f.Path) {
			docsOnly = false
		}
		if infraFile.MatchString(f.Path) {
			infra = true
		}
		if riskyFile.MatchString(f.Path) {
			risky = append(risky, f.Path)
		}
	}

	var p Plan
	switch {
	case docsOnly:
		p = Plan{Tier: "small", AgentModel: "haiku", VerifierModel: "sonnet", Agents: []string{"logic"}}
		p.Reasons = append(p.Reasons, "docs only")
	case lines > largeLines || (len(risky) > 0 && lines > smallLines):
		p = Plan{Tier: "large", AgentModel: "opus", VerifierModel: "opus"}
	case lines > smallLines || len(risky) > 0:
		p = Plan{Tier: "medium", AgentModel: "sonnet", VerifierModel: "opus"}
	default:
		p = Plan{Tier: "small", AgentModel: "haiku", VerifierModel: "sonnet"}
	}
	if !docsOnly {
		p.Agents = []string{"logic", "security", "integration"}
		if infra {
			p.Agents = append(p.Agents, "infra")
		}
	}
	// The design is worth questioning on anything beyond a small change, and on any recorded decision.
	switch {
	case adr:
		p.Design = true
		p.Reasons = append(p.Reasons, "touches an ADR")
	case !docsOnly && p.Tier != "small":
		p.Design = true
	}
	p.Followup, p.FollowupModel = true, "sonnet"
	p.DesignModel = "opus"
	if p.Tier == "large" {
		p.DesignModel = "fable"
	}
	p.Reasons = append(p.Reasons, fmt.Sprintf("%d lines in %d files", lines, len(files)))
	if len(risky) > 0 {
		shown := risky
		if len(shown) > 3 {
			shown = append(shown[:3:3], fmt.Sprintf("+%d more", len(risky)-3))
		}
		p.Reasons = append(p.Reasons, "risky paths: "+strings.Join(shown, ", "))
	}
	if infra {
		p.Reasons = append(p.Reasons, "infrastructure changes")
	}
	return p
}
