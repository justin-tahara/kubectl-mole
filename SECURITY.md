# Security

## Reporting a vulnerability

Report vulnerabilities privately via
[GitHub security advisories](https://github.com/justin-tahara/kubectl-mole/security/advisories/new).
Do not open public issues for security reports.

This is a solo-maintained project. Expect an initial response within a week.

## Threat model notes

- The tool is strictly read-only against the cluster. It never mutates state.
- Log lines and event messages included as evidence in verdicts are
  attacker-controllable text. Output marks every evidence item
  `"untrusted": true`, and consumers — human or agent — must never treat
  evidence content as instructions.
