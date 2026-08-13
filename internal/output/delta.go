package output

import (
	"fmt"
	"sort"
)

// Delta is what moved since a previous verdict (--since). It rides on the
// verdict — the emitted JSON stays a valid --since input for the next run,
// so a monitor loop is one command: run, save, repeat. Excluded from the
// content hash like Elapsed: it describes the pair of runs, not the state.
type Delta struct {
	// SinceHash is the previous verdict's contentHash, for traceability.
	SinceHash string `json:"sinceHash,omitempty"`
	// Baseline marks a run with no previous verdict (the --since file did
	// not exist). A baseline run keeps the normal exit codes: the first
	// run of a loop reports full state, not an empty diff.
	Baseline    bool         `json:"baseline,omitempty"`
	Changed     bool         `json:"changed"`
	Transitions []Transition `json:"transitions"`
}

// Transition kinds. Status transitions read a target leaving the failing
// list of a checked verdict as settled — the verdict counts settled targets
// instead of listing them, and a target removed from the cluster leaves the
// list the same way (scope changes show in fleet.targets).
const (
	// TransitionStatus is a target's (or, when nothing finer was checked,
	// the whole verdict's) status change.
	TransitionStatus = "status"
	// TransitionContext is one kubeconfig context's rollup status change.
	TransitionContext = "context"
	// TransitionNewTermination: the advisory's freshest termination moved —
	// a container was killed and recovered between runs. This is the
	// transition the content hash cannot see (advisories are excluded from
	// it), and the reason delta mode diffs advisories on their own.
	TransitionNewTermination = "new-termination"
	// TransitionAdvisoryAppeared / Cleared: fresh-termination evidence
	// appeared on a previously quiet target, or aged out of the window.
	TransitionAdvisoryAppeared = "advisory-appeared"
	TransitionAdvisoryCleared  = "advisory-cleared"
)

// Transition is one observed change between the previous and current
// verdict. The advisory kinds carry the new termination's stable facts.
type Transition struct {
	Kind      string `json:"kind"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Target    string `json:"target,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	// New-termination facts (advisory kinds only).
	LastReason     string `json:"lastReason,omitempty"`
	LastExitCode   *int32 `json:"lastExitCode,omitempty"`
	LastFinishedAt string `json:"lastFinishedAt,omitempty"`
	Text           string `json:"text"`
}

type entryKey struct{ context, namespace, target string }

// Diff computes the transitions from prev to cur. Target statuses, context
// rollups, and the whole-verdict status diff on the settle state; advisories
// diff separately on their delta-stable fields, because Hash excludes them —
// a kill-and-recover between runs changes only the advisory, and that is
// exactly the event an incident loop watches for.
func Diff(prev, cur Verdict) *Delta {
	d := &Delta{SinceHash: prev.ContentHash, Transitions: []Transition{}}

	// A structural status means the run could not enumerate targets; only a
	// checked verdict's failing list supports "absent = settled".
	checked := func(v Verdict) bool {
		return v.Status != StatusPermissionDenied && v.Status != StatusNoMatch
	}
	pt, ct := targetStatuses(prev), targetStatuses(cur)
	for k, cs := range ct {
		ps, listed := pt[k]
		switch {
		case listed && ps != cs:
			d.add(statusTransition(k, ps, cs))
		case !listed && checked(prev):
			// Absent from a checked previous verdict's failing list means
			// it was settled (or not there yet — same story: newly failing).
			d.add(statusTransition(k, StatusSettled, cs))
		case !listed:
			d.add(statusTransition(k, "", cs))
		}
	}
	if checked(cur) {
		for k, ps := range pt {
			if _, ok := ct[k]; !ok {
				d.add(statusTransition(k, ps, StatusSettled))
			}
		}
	}

	pc, cc := contextStatuses(prev), contextStatuses(cur)
	for name, cs := range cc {
		if ps, ok := pc[name]; ok && ps != cs {
			d.add(Transition{Kind: TransitionContext, Context: name, From: ps, To: cs,
				Text: fmt.Sprintf("context %s: %s -> %s", name, ps, cs)})
		}
	}

	// Whole-verdict status change, when nothing finer told the story — a
	// single-target verdict has no entry lists, and a fleet that flipped to
	// a structural status has nothing to enumerate.
	if len(d.Transitions) == 0 && prev.Status != cur.Status {
		d.add(Transition{Kind: TransitionStatus,
			Context: firstContext(cur), Namespace: cur.Namespace, Target: cur.Target,
			From: prev.Status, To: cur.Status,
			Text: locate(entryKey{firstContext(cur), cur.Namespace, cur.Target}) +
				fmt.Sprintf("%s -> %s", prev.Status, cur.Status)})
	}

	// Advisory transitions — always diffed, hash-blind.
	pa, ca := advisoryIndex(prev), advisoryIndex(cur)
	for k, a := range ca {
		p, ok := pa[k]
		switch {
		case !ok:
			d.add(advisoryTransition(TransitionAdvisoryAppeared, k, a,
				"restart advisory appeared"))
		case newTermination(p, a):
			d.add(advisoryTransition(TransitionNewTermination, k, a,
				"new termination"))
		}
	}
	if checked(cur) {
		for k := range pa {
			if _, ok := ca[k]; ok {
				continue
			}
			if _, failing := ct[k]; failing {
				// The target's story is its status transition; a vanished
				// advisory on a now-failing target is not "aged out".
				continue
			}
			d.add(Transition{Kind: TransitionAdvisoryCleared,
				Context: k.context, Namespace: k.namespace, Target: k.target,
				Text: locate(k) + "restart advisory cleared"})
		}
	}

	sort.SliceStable(d.Transitions, func(i, j int) bool {
		a, b := d.Transitions[i], d.Transitions[j]
		if a.Context != b.Context {
			return a.Context < b.Context
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Kind < b.Kind
	})
	d.Changed = len(d.Transitions) > 0
	return d
}

func (d *Delta) add(t Transition) { d.Transitions = append(d.Transitions, t) }

// newTermination reports whether the advisory's freshest termination moved.
// LastFinishedAt is the identity — absolute, never churning. A previous
// verdict written by a binary without the field falls back to the stable
// facts it does have, so a loop keeps working across an upgrade.
func newTermination(prev, cur Advisory) bool {
	if prev.LastFinishedAt != "" && cur.LastFinishedAt != "" {
		return prev.LastFinishedAt != cur.LastFinishedAt
	}
	if prev.LastExitCode != nil && cur.LastExitCode != nil && *prev.LastExitCode != *cur.LastExitCode {
		return true
	}
	return prev.LastReason != cur.LastReason
}

// targetStatuses indexes the non-settled targets a verdict enumerates.
func targetStatuses(v Verdict) map[entryKey]string {
	m := map[entryKey]string{}
	for _, n := range v.Namespaces {
		for _, t := range n.Targets {
			m[entryKey{n.Context, n.Namespace, t.Target}] = t.Status
		}
	}
	return m
}

func contextStatuses(v Verdict) map[string]string {
	m := map[string]string{}
	for _, c := range v.Contexts {
		m[c.Context] = c.Status
	}
	return m
}

func advisoryIndex(v Verdict) map[entryKey]Advisory {
	m := map[entryKey]Advisory{}
	for _, a := range v.Advisories {
		key := entryKey{a.Context, a.Namespace, a.Target}
		if a.Target == "" {
			// Single-target advisories omit the locator; the verdict
			// header carries it.
			key = entryKey{a.Context, v.Namespace, v.Target}
		}
		m[key] = a
	}
	return m
}

func statusTransition(k entryKey, from, to string) Transition {
	text := locate(k)
	if from == "" {
		text += "now " + to
	} else {
		text += from + " -> " + to
	}
	return Transition{Kind: TransitionStatus,
		Context: k.context, Namespace: k.namespace, Target: k.target,
		From: from, To: to, Text: text}
}

func advisoryTransition(kind string, k entryKey, a Advisory, verb string) Transition {
	facts := ""
	if a.LastExitCode != nil {
		facts = fmt.Sprintf(" (exit %d)", *a.LastExitCode)
		if a.LastReason != "" {
			facts = fmt.Sprintf(" (%s, exit %d)", a.LastReason, *a.LastExitCode)
		}
	}
	return Transition{Kind: kind,
		Context: k.context, Namespace: k.namespace, Target: k.target,
		LastReason: a.LastReason, LastExitCode: a.LastExitCode, LastFinishedAt: a.LastFinishedAt,
		Text: locate(k) + verb + facts}
}

// locate renders the transition's locator prefix, mirroring the advisory
// display rule: target and namespace when known, context first.
func locate(k entryKey) string {
	s := ""
	if k.target != "" {
		s = k.target
		if k.namespace != "" && k.namespace != "*" {
			s += " -n " + k.namespace
		}
		s += ": "
	}
	if k.context != "" {
		s = k.context + ": " + s
	}
	return s
}

func firstContext(v Verdict) string {
	if len(v.Contexts) == 1 {
		return v.Contexts[0].Context
	}
	return ""
}
