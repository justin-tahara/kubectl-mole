package collapse

import (
	"strings"

	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

// maxExamples bounds the refs kept per entry; the Affected count carries the
// rest.
const maxExamples = 3

// Entry is one collapsed failure: every finding that shares a signature and
// cause, folded into a single cause with a count.
type Entry struct {
	Signature string
	Cause     string
	// Chain is the ownership walk of the entry's first finding.
	Chain    []string
	Evidence []signatures.Evidence
	// Affected counts the distinct resources sharing this cause.
	Affected int
	// Examples are namespace-qualified refs of up to maxExamples affected
	// resources, in finding order.
	Examples []string
	// Pods lists every distinct pod anchored to this entry, for summary
	// counting. Empty for workload-level entries.
	Pods []string
}

// Collapse groups findings by signature and cause. A consumer acting on 40
// symptom entries proposes 40 unrelated fixes; one entry with affected: 40
// names the shared cause once. Findings arrive ordered (workload-level
// first, then pods sorted by name) and groups keep first-appearance order,
// so the output is deterministic.
func Collapse(namespace string, findings []signatures.Finding) []Entry {
	entries := map[string]*Entry{}
	anchors := map[string]map[string]bool{}
	var order []string
	for _, f := range findings {
		key := f.Signature + "\x00" + f.Cause
		e, ok := entries[key]
		if !ok {
			e = &Entry{Signature: f.Signature, Cause: f.Cause, Chain: f.Chain, Evidence: f.Evidence}
			entries[key] = e
			anchors[key] = map[string]bool{}
			order = append(order, key)
		}
		ref := anchorRef(namespace, f)
		if anchors[key][ref] {
			continue
		}
		anchors[key][ref] = true
		e.Affected++
		if len(e.Examples) < maxExamples {
			e.Examples = append(e.Examples, ref)
		}
		if f.Pod != "" {
			e.Pods = append(e.Pods, f.Pod)
		}
	}
	out := make([]Entry, 0, len(order))
	for _, key := range order {
		out = append(out, *entries[key])
	}
	return out
}

// anchorRef names the resource a finding is anchored to: the pod for
// pod-level findings, the workload itself for workload-level ones.
func anchorRef(namespace string, f signatures.Finding) string {
	name := f.Pod
	if name == "" && len(f.Chain) > 0 {
		name = f.Chain[0]
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[i+1:]
		}
	}
	return namespace + "/" + name
}
