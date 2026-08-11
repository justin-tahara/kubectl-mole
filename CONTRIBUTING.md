# Contributing

Thanks for your interest. The project is young; right now the fastest way to
help is to open an issue describing a Kubernetes failure mode you want
diagnosed — what broke, what the API objects looked like, and what the verdict
should have said.

## The PR this project is built around

The failure-signature catalogue is the contribution surface. Once the
signature framework lands (M2 in [DESIGN.md](DESIGN.md)), adding a detector is
a small, isolated change: `make new-signature NAME=Foo` scaffolds the
detector, its unit test, and a bench scenario stub. Every new signature PR
must include a reproducible bench scenario — that keeps contributions
self-verifying and grows the benchmark suite for free.

This document gains a full worked example (one signature, end to end) when the
framework lands.

## Ground rules

- Sign your commits off (DCO): `git commit -s`. There is no CLA.
- `make build test vet lint` must pass.
- Read [DESIGN.md](DESIGN.md) before proposing changes to the design
  constraints — they were decided deliberately.
