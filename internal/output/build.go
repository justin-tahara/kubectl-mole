package output

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/justin-tahara/kubectl-mole/internal/collapse"
)

// Input carries everything the builder needs for one watched workload.
type Input struct {
	Kind      string // "Deployment", "StatefulSet", "DaemonSet"
	Name      string
	Namespace string
	Status    string // one of the Status* constants
	Reason    string
	// EarlyExit marks a failure the wedged-for window declared before the
	// timeout; WedgedFor is that window.
	EarlyExit bool
	WedgedFor time.Duration
	Elapsed   time.Duration
	// Pods are the current-revision pods at the end of the watch.
	Pods []*corev1.Pod
	// OldPods are previous-revision pods still present at the end.
	OldPods []*corev1.Pod
	// Failures are the collapsed findings, in collapse order.
	Failures []collapse.Entry
	// Degraded lists reads that were denied and the analysis skipped.
	Degraded []string
	// Advisories are informational notes (fresh restart evidence on a
	// settled workload); the verdict outcome is unaffected.
	Advisories []Advisory
}

// FleetTarget carries one fleet member's outcome into the builder.
type FleetTarget struct {
	Kind      string
	Name      string
	Namespace string
	// Context is the kubeconfig context the target was watched through;
	// empty on single-cluster runs.
	Context string
	Status  string // one of the Status* constants
	Reason  string
	// Pods are the target's current-revision pods at the end of its watch.
	Pods []*corev1.Pod
	// OldPods are the target's previous-revision pods still present.
	OldPods []*corev1.Pod
}

// FleetInput carries everything the builder needs for one fan-out run.
type FleetInput struct {
	// Target overrides the verdict's target field; "" means "workloads".
	// A named target watched across --contexts keeps its own name — the
	// verdict is fleet-shaped (N watches) but about one workload.
	Target string
	// Namespace scoping the run; "" means all namespaces.
	Namespace string
	Selector  string
	// Contexts, when non-empty, marks a --contexts run: one entry per
	// kubeconfig context, sorted by name. An entry with Status "" is
	// filled from its own targets; a pre-filled entry states a
	// context-level outcome (unreachable, permission denied, no match)
	// that produced no targets.
	Contexts []ContextVerdict
	Elapsed  time.Duration
	// Targets in fleet order (context, then namespace, kind, name).
	Targets []FleetTarget
	// EarlyExit marks that at least one target failed through the
	// wedged-for window; WedgedFor is that window.
	EarlyExit bool
	WedgedFor time.Duration
	// Failures are the collapsed findings across the whole fleet.
	Failures []collapse.Entry
	Degraded []string
	// Advisories are informational notes across the fleet's settled
	// targets, each carrying its workload (and context) in the typed
	// fields.
	Advisories []Advisory
}

// Build assembles the schemaVersion "1" verdict. Ordering is inherited from
// the collapse layer (workload-level findings first, then pods sorted by
// name), so the output is deterministic.
func Build(in Input) Verdict {
	v := Verdict{
		SchemaVersion: SchemaVersion,
		Status:        in.Status,
		Target:        in.Kind + "/" + in.Name,
		Namespace:     in.Namespace,
		Reason:        in.Reason,
		Elapsed:       in.Elapsed.Round(time.Second).String(),
		Summary:       summarize(in.Pods, in.OldPods, in.Failures),
		Failures:      buildFailures(in.Failures),
		Degraded:      append([]string{}, in.Degraded...),
		Advisories:    in.Advisories,
	}
	if in.EarlyExit {
		v.EarlyExit = true
		v.WedgedFor = in.WedgedFor.String()
	}
	v.ContentHash = Hash(v)
	return v
}

// BuildFleet assembles the fan-out verdict. Status is the worst outcome in
// the fleet — failed beats progressing beats settled — because the exit code
// derives from it and automation acts on the exit code. On a --contexts run
// the worst is taken across contexts by StatusRank, so a cluster the run
// could not verify can never hide behind a settled or progressing one.
func BuildFleet(in FleetInput) Verdict {
	var pods, oldPods []*corev1.Pod
	for _, t := range in.Targets {
		pods = append(pods, t.Pods...)
		oldPods = append(oldPods, t.OldPods...)
	}
	target := in.Target
	if target == "" {
		target = "workloads"
	}
	status := worstStatus(in.Targets)
	reason := fleetReason(in.Targets)
	contexts := fillContextVerdicts(in.Contexts, in.Targets)
	if len(contexts) > 0 {
		status, reason = contextsStatus(contexts)
	}
	counts := fleetCounts(in.Targets)
	counts.Contexts = len(contexts)
	v := Verdict{
		SchemaVersion: SchemaVersion,
		Status:        status,
		Target:        target,
		Namespace:     fleetNamespace(in.Namespace),
		Selector:      in.Selector,
		Reason:        reason,
		Elapsed:       in.Elapsed.Round(time.Second).String(),
		Summary:       summarize(pods, oldPods, in.Failures),
		Fleet:         counts,
		Contexts:      contexts,
		Namespaces:    namespaceVerdicts(in.Targets),
		Failures:      buildFailures(in.Failures),
		Degraded:      append([]string{}, in.Degraded...),
		Advisories:    in.Advisories,
	}
	if in.EarlyExit {
		v.EarlyExit = true
		v.WedgedFor = in.WedgedFor.String()
	}
	v.ContentHash = Hash(v)
	return v
}

// fillContextVerdicts completes the per-context rollup: an entry the caller
// left with Status "" is judged from its own targets — a lone target lends
// its status and reason verbatim, several fold like a fleet. Pre-filled
// entries (context-level outcomes with no targets) pass through untouched.
func fillContextVerdicts(entries []ContextVerdict, targets []FleetTarget) []ContextVerdict {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ContextVerdict, len(entries))
	for i, e := range entries {
		if e.Status != "" {
			out[i] = e
			continue
		}
		var own []FleetTarget
		for _, t := range targets {
			if t.Context == e.Context {
				own = append(own, t)
			}
		}
		switch len(own) {
		case 0:
			// Never judge an unwatched context settled.
			e.Status = StatusNoMatch
			e.Reason = "no workloads found"
		case 1:
			e.Status = own[0].Status
			e.Reason = own[0].Reason
		default:
			e.Status = worstStatus(own)
			e.Reason = fleetReason(own)
		}
		out[i] = e
	}
	return out
}

// contextsStatus merges the per-context rollup into the verdict's status and
// reason: worst status by StatusRank, and a reason counting the contexts at
// that status — the per-cluster detail lives in the rollup entries.
func contextsStatus(entries []ContextVerdict) (string, string) {
	counts := map[string]int{}
	worst := StatusSettled
	for _, e := range entries {
		counts[e.Status]++
		if StatusRank(e.Status) > StatusRank(worst) {
			worst = e.Status
		}
	}
	n := len(entries)
	switch worst {
	case StatusFailed:
		return worst, fmt.Sprintf("%d of %d contexts failed", counts[worst], n)
	case StatusPermissionDenied:
		return worst, fmt.Sprintf("%d of %d contexts permission denied", counts[worst], n)
	case StatusNoMatch:
		return worst, fmt.Sprintf("%d of %d contexts matched nothing", counts[worst], n)
	case StatusProgressing:
		return worst, fmt.Sprintf("%d of %d contexts still progressing at timeout", counts[worst], n)
	}
	return worst, fmt.Sprintf("all %d contexts settled", n)
}

// NoMatch is the exit-4 verdict: the target matched nothing. kubectl exits 0
// on an empty match and consumers read that as success — this status exists
// to kill that false positive.
func NoMatch(kind, name, namespace, reason string) Verdict {
	return errorVerdict(kind+"/"+name, namespace, "", StatusNoMatch, reason)
}

// PermissionDenied is the exit-3 verdict: RBAC blocked the reads the check
// depends on. The reason names the missing verb and resource; a raw 403 never
// reaches the output.
func PermissionDenied(kind, name, namespace, reason string) Verdict {
	return errorVerdict(kind+"/"+name, namespace, "", StatusPermissionDenied, reason)
}

// NoMatchFleet is the exit-4 verdict for a fan-out that matched nothing.
func NoMatchFleet(namespace, selector, reason string) Verdict {
	return errorVerdict("workloads", fleetNamespace(namespace), selector, StatusNoMatch, reason)
}

// PermissionDeniedFleet is the exit-3 verdict for a fan-out RBAC blocked.
func PermissionDeniedFleet(namespace, selector, reason string) Verdict {
	return errorVerdict("workloads", fleetNamespace(namespace), selector, StatusPermissionDenied, reason)
}

func errorVerdict(target, namespace, selector, status, reason string) Verdict {
	v := Verdict{
		SchemaVersion: SchemaVersion,
		Status:        status,
		Target:        target,
		Namespace:     namespace,
		Selector:      selector,
		Reason:        reason,
		Elapsed:       "0s",
		Failures:      []Failure{},
		Degraded:      []string{},
	}
	v.ContentHash = Hash(v)
	return v
}

func buildFailures(entries []collapse.Entry) []Failure {
	out := []Failure{}
	for _, e := range entries {
		f := Failure{
			Signature: e.Signature,
			Cause:     e.Cause,
			Chain:     strings.Join(e.Chain, " → "),
			Affected:  e.Affected,
			Examples:  append([]string{}, e.Examples...),
			Evidence:  []Evidence{},
		}
		for _, ev := range e.Evidence {
			f.Evidence = append(f.Evidence, Evidence{Source: ev.Source, Untrusted: true, Text: ev.Text})
		}
		out = append(out, f)
	}
	return out
}

// fleetNamespace renders the fan-out scope: "*" is the JSON marker for all
// namespaces (the text formatter spells it out).
func fleetNamespace(ns string) string {
	if ns == "" {
		return "*"
	}
	return ns
}

func worstStatus(targets []FleetTarget) string {
	worst := StatusSettled
	for _, t := range targets {
		switch t.Status {
		case StatusFailed:
			return StatusFailed
		case StatusProgressing:
			worst = StatusProgressing
		}
	}
	return worst
}

func fleetCounts(targets []FleetTarget) *FleetCounts {
	c := &FleetCounts{Targets: len(targets)}
	// Keyed by (context, namespace): the same namespace name in two
	// clusters is two namespaces. Single-cluster contexts are all "".
	namespaces := map[string]bool{}
	for _, t := range targets {
		namespaces[t.Context+"\x00"+t.Namespace] = true
		switch t.Status {
		case StatusSettled:
			c.Settled++
		case StatusFailed:
			c.Failed++
		case StatusProgressing:
			c.Progressing++
		}
	}
	c.Namespaces = len(namespaces)
	return c
}

func fleetReason(targets []FleetTarget) string {
	c := fleetCounts(targets)
	switch {
	case c.Failed > 0:
		return fmt.Sprintf("%d of %d workloads failed", c.Failed, c.Targets)
	case c.Progressing > 0:
		return fmt.Sprintf("%d of %d workloads still progressing at timeout", c.Progressing, c.Targets)
	}
	return fmt.Sprintf("all %d workloads settled", c.Targets)
}

// namespaceVerdicts groups the non-settled targets by (context, namespace):
// the same namespace name in two clusters is two different namespaces.
// Targets arrive in fleet order (context, then namespace, kind, name), so
// entries come out sorted with their targets in kind/name order.
func namespaceVerdicts(targets []FleetTarget) []NamespaceVerdict {
	var out []NamespaceVerdict
	idx := map[string]int{}
	for _, t := range targets {
		if t.Status == StatusSettled {
			continue
		}
		key := t.Context + "\x00" + t.Namespace
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, NamespaceVerdict{Context: t.Context, Namespace: t.Namespace, Status: StatusProgressing, Targets: []TargetVerdict{}})
		}
		if t.Status == StatusFailed {
			out[i].Status = StatusFailed
		}
		out[i].Targets = append(out[i].Targets, TargetVerdict{Target: t.Kind + "/" + t.Name, Status: t.Status, Reason: t.Reason})
	}
	return out
}

func summarize(pods, oldPods []*corev1.Pod, entries []collapse.Entry) Summary {
	s := Summary{Total: len(pods), Old: len(oldPods)}
	for _, p := range pods {
		if podIsReady(p) {
			s.Ready++
		}
	}
	failed := map[string]bool{}
	for _, e := range entries {
		for _, p := range e.Pods {
			failed[p] = true
		}
	}
	s.Failed = len(failed)
	return s
}

func podIsReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
