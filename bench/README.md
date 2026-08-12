# Benchmark methodology

The token claim is this project's headline, so the way the numbers are
produced matters more than the numbers. Everything here is designed so that
nothing favors mole; where a rule has slack, the slack goes to the
baselines. Scenarios mole loses are published like the rest.

## Running it

```
make bench          # pinned cluster up → measure → cluster down
make bench-up       # or manage the lifecycle yourself
make bench-run BENCH_ARGS="--only sig-crashloopbackoff"
make bench-run BENCH_ARGS="--full"    # adds fan-out at 500 and 5000 namespaces
make bench-down
```

Results land in [results.csv](results.csv) and [RESULTS.md](RESULTS.md),
both committed. Raw tool outputs land in `bench/raw/` (gitignored) for
scrutiny of any number.

### Iterating on one scenario

A `--only` run merges into the committed results instead of clobbering
them: the scenarios you measured are replaced, everything else is carried
over, and RESULTS.md notes the merge. With a warm cluster, iterating on
one detector costs minutes:

```
make bench-up                                       # once
make bench-run BENCH_ARGS="--only sig-configmissing"   # ~2 min per loop
make bench-down                                     # when done
```

Quote-worthy numbers (README, release notes) still come from one full run
on a fresh cluster: `make bench BENCH_ARGS=--full`. Git history carries
per-scenario provenance for merged rows.

## What is pinned

- Kubernetes: the kind node image digest in [kind.yaml](kind.yaml)
- Every container image: one busybox digest-stable tag via the ECR Public
  mirror
- `kubectl-status`: the version in the Makefile (`KSTATUS_VERSION`)
- Tokenizer: tiktoken `o200k_base`; raw bytes are always published alongside
  tokens

## The four tools

| Tool | Definition |
|---|---|
| mole | One invocation: `kubectl-mole <target> -o json` with `--stable-for 5s` and a per-scenario `--timeout`. No budget flag: the measured output is the full verdict. |
| expert kubectl | The minimal hand-tuned sequence a good SRE runs for that scenario class, knowing the failure class but not the answer (e.g. `get pods` → `describe pod <failing>` → `logs --previous`). This is the honest comparison. |
| naive kubectl | The flailing loop: `get all -o yaml`, `describe pods`, `get events`, `logs`. |
| kubectl-status | `kubectl status deployments -n <ns>` — the closest neighbour tool. |

The failing pod handed to the baselines is resolved mechanically: the first
(by name) not-ready pod of the workload — the pod an SRE would pick from
`get pods`.

## Fairness rules

- **Baselines are never charged for waiting.** mole's wall-clock includes
  its own watch and stability window, by design. Baseline sequences run
  after the failure state is steady.
- **A failed command still counts.** If `logs` errors, the error text is
  the output — that is what the consumer has to read — and the invocation
  counts.
- **Output is stdout and stderr combined**, for every tool including mole.

## Metrics

- **bytes / tokens** — combined output size; tokens via `o200k_base`.
- **invocations** — commands run to reach the answer. mole is always 1.
- **wall_ms** — summed command wall-clock. See fairness rules: mole's
  number includes its deliberate watch; the baselines' numbers do not
  include the wait for the failure to manifest.
- **truth_found** — whether the combined output contains the ground-truth
  cause. Each scenario defines regexes (in [scenarios.go](scenarios.go));
  all must match, case-insensitively, so a lucky keyword cannot count.
- **signal_density** — the fraction of output tokens on lines that pertain
  to the failure: lines naming a failing resource (workload, pod, node),
  stating the ground-truth cause, or naming the failure class. The same
  mechanical rule for every tool; structural lines count against everyone,
  including mole's JSON scaffolding.
- **mole_est_error_pct** — the signed error of mole's own ~3 chars/token
  budget estimate against the real tokenizer, published because `--budget`
  is documented as advisory. (The constant was 4 before this bench first
  measured it: verdict JSON tokenizes at ~2.9 chars/token, so 4 made every
  budget overshoot by ~25%.)

## Corpus

One scenario per signature (nine), the node-NotReady causal collapse (a
kind worker is `docker pause`d under two workloads — the right answer names
the node once, not four pod symptoms), a healthy control, and fan-out at
50, 500, and 5,000 namespaces (each: n−3 settled zero-replica fleets plus 3
identical crashers; the 5,000 point sits exactly at the default
`--max-targets` ceiling). The large fan-out points run with `--full`.

More than a third of the corpus is small and single-namespace on purpose:
the default path is one namespace and a handful of resources.

## CI growth gate

The `bench` CI job re-measures mole on the default corpus and fails when
any scenario's output exceeds committed bytes × 1.30 + 256. Silent output
growth is a regression in the product's headline claim; growing the output
deliberately means re-running `make bench` and committing the new
results.csv in the same PR.
