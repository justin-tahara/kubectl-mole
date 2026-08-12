## What

<!-- One paragraph: what changes and why. Link the issue if one exists. -->

## Checklist

- [ ] Commits are signed off (`git commit -s` — DCO, no CLA)
- [ ] `make build test vet lint` passes
- [ ] e2e updated and passing if behavior changed (`make kind-up && make e2e`)
- [ ] New signature? Detector + priority slot + unit test + e2e scenario +
      bench scenario + docs enum — see [CONTRIBUTING.md](https://github.com/justin-tahara/kubectl-mole/blob/main/CONTRIBUTING.md)
- [ ] Docs updated (README, docs/AGENTS.md) if flags or output changed

## Output diff

<!-- If verdict output changed in any mode (plain text, styled, JSON):
     update the goldens (go test ./internal/output/ -run TestTextGoldens -update)
     and paste the before/after here. Determinism is the product; output
     diffs get read closely. Delete this section if output is unchanged. -->
