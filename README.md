# kubectl-mole

> digs down to what actually broke

**Status: pre-alpha (M6).** Settle detection, the failure-signature
catalogue, the versioned output schema, the full exit-code taxonomy, causal
collapse, the token budget, and namespace fan-out are implemented and tested
against kind. Not yet done: benchmarks — see [DESIGN.md](DESIGN.md) for the
milestone order.

`kubectl mole` watches Kubernetes resources until they settle, then emits one
structured verdict explaining what happened and — if something failed — why.
Deterministic, read-only, no LLM required. Works in your terminal, your CI,
and your agent's context window.

## The problem

`kubectl rollout status` tells you a rollout failed; it does not tell you why.
Finding out means `describe` → `logs` → `get events`, and inferring the causal
chain from scattered fragments yourself. mole collapses that loop into one
command with a definite answer and a meaningful exit code.

## Install (from source)

```
go install github.com/justin-tahara/kubectl-mole/cmd/kubectl-mole@latest
```

Put the binary on your `PATH` and `kubectl mole` works as a plugin. Krew and
release binaries come once the tool does something useful.

## Use

```
kubectl mole deployment/api -n prod
kubectl mole sts/db --timeout 3m --stable-for 20s -o json
kubectl mole deployment/api -n prod -o json --budget 800
kubectl mole -n prod -l app.kubernetes.io/name=api
kubectl mole --all-namespaces -l app.kubernetes.io/part-of=platform
```

The command watches the workload until it settles or the timeout passes, then
prints one verdict. `-o json` emits `schemaVersion: "1"` — field names are
stable, changes are additive.

Without a `TYPE/NAME` argument it fans out: every Deployment, StatefulSet
and DaemonSet in scope, optionally filtered by `-l`, watched together off
one shared informer set. The verdict is still one verdict — worst outcome
wins, identical causes collapse across namespaces into a single entry with a
count, and healthy workloads are counted, never enumerated. A selection
matching more than `--max-targets` workloads (default 5,000) is refused with
a request for a narrower selector. Exit codes:

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

Consuming the output from an agent or CI? Read
[docs/AGENTS.md](docs/AGENTS.md).

## A note on evidence

Verdict output will include log lines and event messages from the cluster.
That text is attacker-controllable: a container can log instructions aimed at
whoever — or whatever — reads the output. Every evidence item is marked
`"untrusted": true` and must never be treated as instructions. This matters
doubly if an agent consumes the output.

## License

Apache 2.0. See [LICENSE](LICENSE).
