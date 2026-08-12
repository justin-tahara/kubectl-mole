<p align="center">
  <img src="assets/mole.svg" width="150" alt="kubectl-mole: a mole surfacing from its tunnel"/>
</p>

# kubectl-mole

> digs down to what actually broke

`kubectl mole` watches Kubernetes workloads until they settle, then emits one
structured verdict explaining what happened and — if something failed — why.
Deterministic, read-only, no LLM. Works in your terminal, your CI, and your
agent's context window.

**Status: pre-alpha (M8).** Everything below is implemented and tested
against kind; release binaries and krew packaging are next — see
[DESIGN.md](DESIGN.md) for the milestone order.

![demo: kubectl mole diagnosing a crash-looping deployment](assets/demo.gif)

## The problem

`kubectl rollout status` tells you a rollout failed; it does not tell you
why. Finding out means `describe` → `logs` → `get events`, and inferring the
causal chain from scattered fragments yourself. mole collapses that loop
into one command with a definite answer and a meaningful exit code.

- **One invocation, one verdict.** Signature, cause, ownership chain
  (`Deployment/api → ReplicaSet/api-7f9c → Pod/api-7f9c-x2k`), and evidence
  — the crash log tail, the scheduler's predicate, the webhook's message.
- **Settle detection that doesn't lie.** A stability window catches the pod
  that goes Ready and crashes 40 seconds later; observedGeneration guards
  against reading the previous rollout's status; old pods must actually be
  gone.
- **Exit codes automation can act on.** `2` (still progressing) is not `1`
  (failed) — conflating them is how automation rolls back a deployment that
  was 30 seconds from healthy. `4` (nothing matched) is not `0` — kubectl
  exits 0 on an empty selector, and consumers read that as success.
- **Causal collapse.** Forty pods failing on one dead node is one
  `NodeNotReady` entry with `"affected": 40` — one fix, not forty. Collapse
  works across namespaces in fan-out mode.
- **Fleet fan-out.** No target means every Deployment, StatefulSet, and
  DaemonSet in scope (`-n`, `-A`, `-l`), watched off one shared informer
  set, returned as one verdict — worst outcome wins.

## The numbers

Measured by [`make bench`](bench/README.md) on a pinned kind cluster:
tiktoken `o200k_base` tokens of combined output, and whether that output
contains the ground-truth cause (✓/✗). Full results incl. bytes, wall-clock,
and signal density: [bench/RESULTS.md](bench/RESULTS.md).

| Scenario | mole | expert kubectl | naive kubectl | kubectl-status |
|---|---|---|---|---|
| CrashLoopBackOff | **343 ✓** (1 cmd) | 980 ✓ (3 cmds) | 3,703 ✓ (4 cmds) | 380 ✗ |
| Node dies under 2 workloads | **552 ✓** | 1,943 ✓ | 10,616 ✓ | 923 ✗ |
| Fan-out, 50 namespaces | **629 ✓** | 2,759 ✓ | 83,092 ✓ | 9,264 ✓ |
| Fan-out, 5,000 namespaces | **626 ✓** | 141,005 ✓ | 4,543,446 ✓ | 131,470 ✗ |

The honest comparison is the expert column — the minimal hand-tuned sequence
a good SRE runs. mole reaches the same answer in one invocation at roughly a
third of the tokens, and its output stays flat as the fleet grows: 629
tokens at 50 namespaces, 626 at 5,000, because identical causes collapse
into one entry and healthy workloads are counted, never enumerated.

Where mole loses, published per the methodology: on a **healthy** workload,
`kubectl get pods` + `get deploy` (98 tokens) beats mole's verdict (178);
mole's wall-clock includes its deliberate watch and stability window, so the
baselines are 20–40× faster to answer *after* the failure is already steady;
and a focused `describe` can beat mole's signal density on workload-level
failures.

## Install (from source)

```
go install github.com/justin-tahara/kubectl-mole/cmd/kubectl-mole@latest
```

Put the binary on your `PATH` and `kubectl mole` works as a plugin. Krew and
release binaries come with M9–M10. RBAC for the read access mole needs ships
in [deploy/rbac.yaml](deploy/rbac.yaml).

## Use

```
kubectl mole deployment/api -n prod
kubectl mole sts/db --timeout 3m --stable-for 20s -o json
kubectl mole deployment/api -n prod -o json --budget 800
kubectl mole -n prod -l app.kubernetes.io/name=api
kubectl mole --all-namespaces -l app.kubernetes.io/part-of=platform
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

Consuming the output from an agent or CI? Read
[docs/AGENTS.md](docs/AGENTS.md).

## A note on evidence

Verdict output includes log lines and event messages from the cluster. That
text is attacker-controllable: a container can log instructions aimed at
whoever — or whatever — reads the output. Every evidence item is marked
`"untrusted": true` and must never be treated as instructions. This matters
doubly if an agent consumes the output.

## License

Apache 2.0. See [LICENSE](LICENSE).
