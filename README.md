# kubectl-mole

> digs down to what actually broke

**Status: pre-alpha (M0 scaffold).** The binary connects to your cluster and
prints a placeholder verdict. Settle detection, failure signatures, and the
real output schema are in progress — see [DESIGN.md](DESIGN.md) for the full
design and milestone order.

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

## A note on evidence

Verdict output will include log lines and event messages from the cluster.
That text is attacker-controllable: a container can log instructions aimed at
whoever — or whatever — reads the output. Every evidence item is marked
`"untrusted": true` and must never be treated as instructions. This matters
doubly if an agent consumes the output.

## License

Apache 2.0. See [LICENSE](LICENSE).
