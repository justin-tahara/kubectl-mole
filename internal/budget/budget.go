package budget

import (
	"encoding/json"

	"github.com/justin-tahara/kubectl-mole/internal/output"
)

// charsPerToken is the deliberately tokenizer-free estimate: roughly four
// characters per token. --budget is approximate and advisory, and documented
// as such.
const charsPerToken = 4

// Tokens estimates the verdict's output cost, measured over the indented
// JSON encoding the CLI emits.
func Tokens(v output.Verdict) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// A struct of plain values cannot fail to marshal.
		return 0
	}
	return (len(b) + charsPerToken - 1) / charsPerToken
}

// Apply trims the verdict to approximately fit the token budget, in tier
// order. The skeleton — status, counts, degraded, truncated — is always
// emitted, even when it alone exceeds the budget. Failure entries are kept
// while they fit, dropped from the end. Evidence returns round-robin so
// every kept failure carries its first item before any carries its second;
// an item that does not fit is skipped, not a stop signal. Every drop is
// counted in truncated: silent truncation makes a partial picture look
// complete.
//
// During trimming the verdict is measured with its original hash in place —
// the hash string's length is constant, so the measurement holds — and
// rehashed once at the end.
func Apply(v output.Verdict, tokens int) output.Verdict {
	if tokens <= 0 || Tokens(v) <= tokens {
		return v
	}
	full := v.Failures

	// Tier 1: evidence-free failure entries, dropped from the end until the
	// verdict fits.
	v.Failures = make([]output.Failure, len(full))
	for i, f := range full {
		f.Evidence = []output.Evidence{}
		v.Failures[i] = f
	}
	for len(v.Failures) > 0 && Tokens(v) > tokens {
		v.Failures = v.Failures[:len(v.Failures)-1]
	}

	// Tier 2: evidence back onto the kept entries, round-robin, first-fit.
	maxEvidence := 0
	for i := range v.Failures {
		if n := len(full[i].Evidence); n > maxEvidence {
			maxEvidence = n
		}
	}
	for round := 0; round < maxEvidence; round++ {
		for i := range v.Failures {
			if round >= len(full[i].Evidence) {
				continue
			}
			v.Failures[i].Evidence = append(v.Failures[i].Evidence, full[i].Evidence[round])
			if Tokens(v) > tokens {
				v.Failures[i].Evidence = v.Failures[i].Evidence[:len(v.Failures[i].Evidence)-1]
			}
		}
	}

	// Truncation accounting: dropped entries count as failures; evidence is
	// counted only for entries that stayed (a dropped entry takes its
	// evidence with it).
	v.Truncated.Failures = len(full) - len(v.Failures)
	v.Truncated.Evidence = 0
	for i := range v.Failures {
		v.Truncated.Evidence += len(full[i].Evidence) - len(v.Failures[i].Evidence)
	}
	v.ContentHash = output.Hash(v)
	return v
}
