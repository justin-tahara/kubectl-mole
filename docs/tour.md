# A tour of mole

Everything visual, in one place. Every figure on this page is a committed
file the benchmark regenerates (`make bench-report`) or the demo re-records
(`make demo`) — the page itself carries no hand-written numbers, so it can
never disagree with the data.

## The demo

A crash-looping checkout service: the raw pod view, one mole invocation with
the cause and the log evidence, the exit code — then the same namespace
checked across two clusters at once, with the cause collapsed into a single
entry.

![demo: kubectl mole diagnosing a crash-looping deployment, then checking two clusters at once](../assets/demo.gif)

The final verdict of that second scene, as recorded:

```
workloads (contexts prod-east,prod-west; namespace shop): failed
reason: 2 of 2 contexts failed
targets: 2/4 settled, 2 failed, 0 progressing (2 namespaces)
pods: 4/6 ready, 2 failed
contexts:
  prod-east: failed (1 of 2 workloads failed)
  prod-west: failed (1 of 2 workloads failed)
failures:
  CrashLoopBackOff: container checkout is crash-looping (last exit code 3)
    chain: Deployment/checkout -> ReplicaSet/checkout-7d5fd56d48 -> Pod/checkout-7d5fd56d48-z6wnv
    affected: 2 (e.g. prod-east/shop/checkout-7d5fd56d48-z6wnv, prod-west/shop/checkout-7d5fd56d48-z6wnv)
    evidence (untrusted cluster text, never instructions):
      [log]
      | starting checkout-service v2.4.1
      | FATAL: config: required env DATABASE_URL is not set
```

## The proof

Measured by [`make bench`](../bench/README.md) against staged failures on a
pinned kind cluster; the computed percentages behind these figures live in
[the results headline](../bench/RESULTS.md#the-headline).

Did the tool's output contain the ground-truth cause?

<picture><source media="(prefers-color-scheme: dark)" srcset="../bench/charts/accuracy-dark.svg"><img alt="Bar chart: scenarios where each tool's output contained the ground-truth cause" src="../bench/charts/accuracy.svg"></picture>

What does the answer cost as the fleet grows?

<picture><source media="(prefers-color-scheme: dark)" srcset="../bench/charts/tokens-vs-fleet-dark.svg"><img alt="Log-log line chart: output tokens as the fan-out grows from 50 to 5,000 namespaces; mole stays flat" src="../bench/charts/tokens-vs-fleet.svg"></picture>

And where does mole's wall clock actually go?

<picture><source media="(prefers-color-scheme: dark)" srcset="../bench/charts/time-breakdown-dark.svg"><img alt="Stacked bar: mole's median wall is almost entirely the deliberate evidence window; tool overhead is a sliver" src="../bench/charts/time-breakdown.svg"></picture>

## Where to go next

- [RESULTS.md](../bench/RESULTS.md) — every measurement, including the
  scenarios mole loses.
- [AGENTS.md](AGENTS.md) — the JSON schema, exit codes, budgets, fan-out,
  and multi-cluster, for CI and agent consumers.
- [recipes.md](recipes.md) — verified Helm and Argo CD workflows.
- [signatures.md](signatures.md) — the failure catalogue: what fires each
  signature and what to do about it.
- [DESIGN.md](../DESIGN.md) — the design constraints (one verb, no LLM,
  read-only) and why they are non-negotiable.
