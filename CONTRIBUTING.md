# Contributing

Thanks for your interest. Two contributions help most:

- **Report a failure mode** mole misses or misdiagnoses — open a
  [signature request](https://github.com/justin-tahara/kubectl-mole/issues/new?template=signature_request.yml)
  describing what broke, what the API objects looked like, and what the
  verdict should have said. A reproducible manifest is gold.
- **Add a signature.** The failure-signature catalogue is the contribution
  surface: detectors are small, isolated, and self-verifying. The anatomy
  is below.

## Development setup

Go (version in `go.mod`), [kind](https://kind.sigs.k8s.io/) and Docker for
the e2e tests, golangci-lint v2 for `make lint`.

| Command | What it does |
|---|---|
| `make build` | Binary at `bin/kubectl-mole`. |
| `make test` | Unit tests. |
| `make vet` / `make lint` | `go vet` / golangci-lint (config: `.golangci.yml`). |
| `make kind-up` → `make e2e` → `make kind-down` | e2e suite against a disposable kind cluster. |
| `make bench` | Full benchmark lifecycle (slow; see below). |

The kind clusters write their kubeconfig inside the repo (gitignored
`.kube/`), never to `~/.kube/config`, and the e2e tests refuse to run
unless `MOLE_E2E_CONTEXT` names the target context explicitly.

## Anatomy of a signature

A new failure signature touches six places. `ConfigMissing`
(`internal/signatures/configerror.go`) is a small, complete example to
crib from.

1. **Detector** — one file in `internal/signatures/`, one function
   `func(c *Context, pod *corev1.Pod) *Finding`. Return `nil` when the pod
   does not match. A `Finding` carries a stable `Signature` name and a
   one-line `Cause` that names the broken object; pull evidence (crash
   logs, events) through the `Context` fetchers, which degrade gracefully
   when RBAC denies a read.
2. **Registration** — a slot in `podDetectors` in
   `internal/signatures/signature.go`. Order is priority: first match
   wins, so deeper causes go before the symptoms they produce (a dead node
   before the unschedulable pod it strands; OOMKilled before the crash
   loop it causes).
3. **Unit test** — table-driven, beside the detector
   (`signatures_test.go` and `everyday_test.go` show the pattern).
4. **e2e scenario** — stage the real failure in kind
   (`internal/signatures/e2e_test.go`) and assert the verdict. This is
   the test that catches detectors that only work on synthetic objects —
   three of the current sixteen signatures were corrected by it.
5. **Bench scenario** — when the failure can be staged deterministically:
   a scenario plus ground-truth regexes in `bench/scenarios.go`, and the
   signature's terms in `bench/tools.go`. Re-measure just your scenario
   into the committed baselines with
   `make bench-up`, then `make bench-run BENCH_ARGS="--only sig-<name>"`.
6. **Docs** — the signature enum in `docs/AGENTS.md`, and README if
   user-visible behavior changed.

## PR flow

`main` is protected: no direct pushes, no force pushes. Every change —
including from the maintainer — lands through a PR with the required
checks green: `test`, `lint` (which also validates the release config),
`e2e`, `licenses`, and `dco`. The bench growth gate additionally runs when
a change could move mole's output (code, harness, or committed baselines).

- Sign off every commit: `git commit -s` (DCO). There is no CLA.
- Keep a PR to one concern; squash or rebase merge keeps history linear.
- If your change alters output, update the goldens
  (`go test ./internal/output/ -run TestTextGoldens -update`) and call the
  diff out in the PR — determinism is the product, so output diffs get
  read closely.

## Design constraints

Read [DESIGN.md](DESIGN.md) before proposing changes to the constraints.
One verb, read-only, informers only (no kubectl shell-outs), no LLM,
deterministic output — these were decided deliberately, and PRs that relax
them will be declined regardless of code quality.

## Security

Vulnerabilities go through
[private security advisories](https://github.com/justin-tahara/kubectl-mole/security/advisories/new),
never public issues — see [SECURITY.md](SECURITY.md).
