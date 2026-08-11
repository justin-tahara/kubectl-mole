# bench

Benchmark harness (M7). `make bench` will spin up a pinned kind cluster, run
every failure scenario against every baseline (naive kubectl, expert kubectl,
`bergerx/kubectl-status`), and emit a CSV plus a markdown table. Methodology
in [DESIGN.md](../DESIGN.md#benchmarks).
