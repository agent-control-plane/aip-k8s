## Summary

<!-- 1-3 bullets covering what changed and why. -->
-

## Linked issues

<!--
Use GitHub closing keywords so the issue auto-closes on merge.
Examples: "Closes #42", "Fixes #7, fixes #8"
-->
Closes #

## Type of change

- [ ] Bug fix
- [ ] New feature / enhancement
- [ ] Refactor (no behavior change)
- [ ] Docs / tests only
- [ ] Chore (build, CI, tooling)

## Test plan

<!-- How was this validated? Check all that apply. -->
- [ ] Unit tests added / updated (`go test ./...`)
- [ ] Integration tests pass (`make test`)
- [ ] e2e tests pass (`make test-e2e`)
- [ ] Manually verified against a kind cluster
- [ ] No tests needed (docs/chore)

## Checklist

- [ ] `go vet ./...` passes
- [ ] `golangci-lint run` passes
- [ ] CRD manifests regenerated if types changed (`make generate manifests`)
- [ ] Breaking API changes documented
