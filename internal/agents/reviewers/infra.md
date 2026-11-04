---
area: infrastructure and rollout
paths: (?i)(\.(tf|tfvars|hcl)$|terragrunt|(^|/)(flux|argocd)/|kustomization\.ya?ml$|(^|/)(charts?|helm|k8s|kubernetes|manifests)/|(^|/)Dockerfile|\.github/workflows/)
---
Terraform/Terragrunt, Kubernetes (manifests, Kustomize, Flux or Argo CD), Helm and CI changes: resources that get destroyed or replaced, state moves, wrong environment or account, prod impact, missing dependencies or ordering, rollout/rollback risk, drift from what's deployed.
