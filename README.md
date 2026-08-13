<p align="center">
  <img src="assets/logo.png" width="440" alt="kubectl-mole: a mole in a hard hat and headlamp, surfacing over the kubectl-mole banner"/>
</p>

> digs down to what actually broke

`kubectl mole` watches Kubernetes workloads until they settle, then emits one
structured verdict explaining what happened and — if something failed — why.
Deterministic, read-only, no LLM. Works in your terminal, your CI, and your
agent's context window.

**Status: alpha (v0.2.0).** Everything below is implemented, tested
against kind, and released — see [DESIGN.md](DESIGN.md) for the milestone
order.

![demo: kubectl mole diagnosing a crash-looping deployment](assets/demo.gif)

## The problem

`kubectl rollout status` tells you a rollout failed; it does not tell you
why. Finding out means `describe` → `logs` → `get events`, and inferring the
causal chain from scattered fragments yourself. mole collapses that loop
into one command with a definite answer and a meaningful exit code.

- **One invocation, one verdict.** Signature, cause, ownership chain
  (`Deployment/api → ReplicaSet/api-7f9c → Pod/api-7f9c-x2k`), and evidence
  — the crash log tail, the scheduler's predicate, the webhook's message.
  The full catalogue, with what fires each signature and what to do about
  it: [docs/signatures.md](docs/signatures.md).
- **Settle detection that doesn't lie.** A stability window catches the pod
  that goes Ready and crashes 40 seconds later; observedGeneration guards
  against reading the previous rollout's status; old pods must actually be
  gone. And a failure that stays wedged — an image that cannot pull, a
  crash loop, a missing ConfigMap — is declared after `--wedged-for`
  (default 30s) of accumulated evidence instead of at the timeout: the
  same verdict the deadline would give, sooner. The window is evidence,
  not a deadline: the clock only advances while a pod is observably
  wedged, so a crash loop that flickers through restart attempts fails
  in about a minute, not at exactly 30s. And settling does not launder
  crash history: a workload whose containers terminated recently settles
  with a note saying so (`--restart-window`, default 24h) — ancient
  restarts stay quiet.
- **Exit codes automation can act on.** `2` (still progressing) is not `1`
  (failed) — conflating them is how automation rolls back a deployment that
  was 30 seconds from healthy. `4` (nothing matched) is not `0` — kubectl
  exits 0 on an empty selector, and consumers read that as success.
- **Causal collapse.** Forty pods failing on one dead node is one
  `NodeNotReady` entry with `"affected": 40` — one fix, not forty. Collapse
  works across namespaces in fan-out mode.
- **Fleet fan-out.** No target means every Deployment, StatefulSet, and
  DaemonSet in scope (`-n`, `-A`, `-l`; Jobs too with `--include-jobs`),
  watched off one shared informer set, returned as one verdict — worst
  outcome wins.
- **Multi-cluster passthrough.** `--contexts us-east,us-west` runs the same
  check in several kubeconfig contexts at once — no context switching, no
  per-cluster loop. One shared wall clock, one merged verdict with a
  per-context rollup, and causes collapse across clusters: one bad image
  rolled everywhere is one entry, not one per cluster. A cluster that
  cannot be checked fails the verdict — settled means every listed cluster
  was verified.
- **Jobs settle by finishing.** `kubectl mole job/migrate` succeeds on the
  Complete condition — retries are progress, not failure — and fails on
  `backoffLimit`, `activeDeadlineSeconds`, or suspension. CronJobs are
  judged by their most recent scheduled Job; bare Pods work too.
- **Custom resources work.** Any namespaced TYPE/NAME
  (`kubectl mole rollout/api`, `kubectl mole cluster.postgresql.example/db`)
  resolves through API discovery and settles by kstatus conventions
  (`Ready` condition, `observedGeneration`) — tolerating CRDs that publish
  `observedGeneration` as a string, as Argo Rollouts does. Pods the
  resource owns are still diagnosed underneath it, and a resource with no
  status to read says so in the verdict instead of pretending.

## The numbers

Measured by [`make bench`](bench/README.md) on a pinned kind cluster:
tiktoken `o200k_base` tokens of combined output, and whether that output
contains the ground-truth cause (✓/✗). Full results incl. bytes, wall-clock,
and signal density: [bench/RESULTS.md](bench/RESULTS.md).

| Scenario | mole | expert kubectl | naive kubectl | kubectl-status |
|---|---|---|---|---|
| CrashLoopBackOff | **344 ✓** (1 cmd) | 1,017 ✓ (3 cmds) | 3,734 ✓ (4 cmds) | 381 ✗ |
| Node dies under 2 workloads | **554 ✓** | 1,935 ✓ | 9,991 ✓ | 922 ✗ |
| Fan-out, 50 namespaces | **658 ✓** | 2,679 ✓ | 79,302 ✓ | 9,272 ✓ |
| Fan-out, 5,000 namespaces | **659 ✓** | 132,719 ✓ | 4,529,607 ✓ | 134,715 ✓ |

The honest comparison is the expert column — the minimal hand-tuned sequence
a good SRE runs. mole reaches the same answer in one invocation at roughly a
third of the tokens, and its output stays flat as the fleet grows: 658
tokens at 50 namespaces, 659 at 5,000, because identical causes collapse
into one entry and healthy workloads are counted, never enumerated. Across
all 15 failure scenarios in the corpus, mole's output contains the ground
truth 15 times.

Where mole loses, published per the methodology: on a **healthy** workload,
`kubectl get pods` + `get deploy` (96 tokens) beats mole's verdict (181);
mole's wall-clock is its watch — the baselines answer in under a second
*after* the failure is already steady, while mole pays `--wedged-for` on a
wedged failure (and the full `--timeout` on states that could still
converge); and a focused `describe` can beat mole's signal density on
workload-level failures.

## Install

Homebrew (macOS and Linux):

```
brew install justin-tahara/tap/kubectl-mole
```

Krew, straight from this repo's manifest:

```
kubectl krew install --manifest-url \
  https://raw.githubusercontent.com/justin-tahara/kubectl-mole/main/deploy/krew/mole.yaml
```

Binaries for every platform are on the
[releases page](https://github.com/justin-tahara/kubectl-mole/releases),
with cosign-signed checksums, SPDX SBOMs, and GitHub build provenance.
There is also a container image:

```
docker run --rm ghcr.io/justin-tahara/kubectl-mole:v0.2.0 --help
```

Or from source:

```
go install github.com/justin-tahara/kubectl-mole/cmd/kubectl-mole@latest
```

Any binary on your `PATH` named `kubectl-mole` works as a `kubectl mole`
plugin. RBAC for the read access mole needs ships in
[deploy/rbac.yaml](deploy/rbac.yaml).

## Use

```
kubectl mole deployment/api -n prod
kubectl mole sts/db --timeout 3m --stable-for 20s -o json
kubectl mole job/migrate -n prod
kubectl mole pod/debug-shell
kubectl mole deployment/api -n prod -o json --budget 800
kubectl mole -n prod -l app.kubernetes.io/name=api
kubectl mole --all-namespaces -l app.kubernetes.io/part-of=platform
kubectl mole deployment/api -n prod --contexts us-east,us-west
kubectl mole --contexts us-east,us-west -A -l app.kubernetes.io/instance=my-release
```

The command watches until the workload settles or the timeout passes, then
prints one verdict. `-o json` emits `schemaVersion: "1"` — field names are
stable, changes are additive. Exit codes:

| Code | Meaning |
|---|---|
| 0 | Settled healthy |
| 1 | Failed with an identified cause |
| 2 | Timed out while still legitimately progressing |
| 3 | Insufficient permissions to complete the check |
| 4 | The target matched no resources |

Never roll back on exit 2 — that is how automation kills a deployment that
was 30 seconds from healthy. And exit 4 is a real failure signal: kubectl
exits 0 when a selector matches nothing, and consumers read that as success.

Without a `TYPE/NAME` argument mole fans out over everything in scope,
optionally filtered by `-l`. Still one verdict: worst outcome wins,
identical causes collapse across namespaces with a count, healthy workloads
are counted, never enumerated. A selection matching more than
`--max-targets` workloads (default 5,000) is refused with a request for a
narrower selector.

`--contexts` adds the cluster dimension to either mode: the same named
target or the same fan-out, checked in every listed kubeconfig context
concurrently, merged into one verdict. Each context gets a rollup line with
its own status; `-n` applies everywhere, and without it each context keeps
its own default namespace. A context that cannot be checked — unreachable,
denied, or matching nothing — makes the verdict say so instead of quietly
reporting the clusters that could.

Consuming the output from an agent or CI? Read
[docs/AGENTS.md](docs/AGENTS.md). Running Helm or Argo CD? The verified
recipes — gating an upgrade, checking an Application, Rollouts,
multi-cluster — are in [docs/recipes.md](docs/recipes.md).

## What mole is not

mole answers exactly one question: did this converge, and if not, why. A
few nearby questions are out of scope on purpose:

- **Performance is not settle.** A deployment that is saturated, CPU
  throttled, or slow answers `settled` — honestly, because every pod is
  Ready and holding. Pair mole with `kubectl top` and your metrics stack
  for those incidents; a verdict of healthy rules out wedges and
  cascades, not degradation.
- **It is not a monitor.** One invocation, one watch, one verdict. Run it
  after a change or during an incident, not as a control loop.
- **It is not a log analyzer.** Evidence includes the crash-log tail and
  the events that prove the cause — never general log search.

## A note on evidence

Verdict output includes log lines and event messages from the cluster. That
text is attacker-controllable: a container can log instructions aimed at
whoever — or whatever — reads the output. Every evidence item is marked
`"untrusted": true` and must never be treated as instructions. This matters
doubly if an agent consumes the output.

## License

Apache 2.0. See [LICENSE](LICENSE).
