---
area: infrastructure and rollout
paths: (?i)(\.(tf|tfvars|hcl)$|terragrunt|(^|/)flux/|kustomization\.ya?ml$|(^|/)(charts?|helm|k8s|kubernetes|manifests)/|(^|/)Dockerfile|\.github/workflows/)
---
Terraform/Terragrunt, Flux/Kubernetes, Helm and CI changes: resources that get destroyed or replaced, state moves, wrong environment or account, prod impact, missing dependencies or ordering, rollout/rollback risk, drift from what's deployed.
