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
	Elapsed   time.Duration
	// Pods are the current-revision pods at the end of the watch.
	Pods []*corev1.Pod
	// Failures are the collapsed findings, in collapse order.
	Failures []collapse.Entry
	// Degraded lists reads that were denied and the analysis skipped.
	Degraded []string
}

// FleetTarget carries one fleet member's outcome into the builder.
type FleetTarget struct {
	Kind      string
	Name      string
	Namespace string
	Status    string // one of the Status* constants
	Reason    string
	// Pods are the target's current-revision pods at the end of its watch.
	Pods []*corev1.Pod
}

// FleetInput carries everything the builder needs for one fan-out run.
type FleetInput struct {
	// Namespace scoping the run; "" means all namespaces.
	Namespace string
	Selector  string
	Elapsed   time.Duration
	// Targets in fleet order (namespace, kind, name).
	Targets []FleetTarget
	// Failures are the collapsed findings across the whole fleet.
	Failures []collapse.Entry
	Degraded []string
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
		Summary:       summarize(in.Pods, in.Failures),
		Failures:      buildFailures(in.Failures),
		Degraded:      append([]string{}, in.Degraded...),
	}
	v.ContentHash = Hash(v)
	return v
}

// BuildFleet assembles the fan-out verdict. Status is the worst outcome in
// the fleet — failed beats progressing beats settled — because the exit code
// derives from it and automation acts on the exit code.
func BuildFleet(in FleetInput) Verdict {
	var pods []*corev1.Pod
	for _, t := range in.Targets {
		pods = append(pods, t.Pods...)
	}
	v := Verdict{
		SchemaVersion: SchemaVersion,
		Status:        worstStatus(in.Targets),
		Target:        "workloads",
		Namespace:     fleetNamespace(in.Namespace),
		Selector:      in.Selector,
		Reason:        fleetReason(in.Targets),
		Elapsed:       in.Elapsed.Round(time.Second).String(),
		Summary:       summarize(pods, in.Failures),
		Fleet:         fleetCounts(in.Targets),
		Namespaces:    namespaceVerdicts(in.Targets),
		Failures:      buildFailures(in.Failures),
		Degraded:      append([]string{}, in.Degraded...),
	}
	v.ContentHash = Hash(v)
	return v
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
	namespaces := map[string]bool{}
	for _, t := range targets {
		namespaces[t.Namespace] = true
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

// namespaceVerdicts groups the non-settled targets by namespace. Targets
// arrive in fleet order (namespace, kind, name), so entries come out sorted
// by namespace with their targets in kind/name order.
func namespaceVerdicts(targets []FleetTarget) []NamespaceVerdict {
	var out []NamespaceVerdict
	idx := map[string]int{}
	for _, t := range targets {
		if t.Status == StatusSettled {
			continue
		}
		i, ok := idx[t.Namespace]
		if !ok {
			i = len(out)
			idx[t.Namespace] = i
			out = append(out, NamespaceVerdict{Namespace: t.Namespace, Status: StatusProgressing, Targets: []TargetVerdict{}})
		}
		if t.Status == StatusFailed {
			out[i].Status = StatusFailed
		}
		out[i].Targets = append(out[i].Targets, TargetVerdict{Target: t.Kind + "/" + t.Name, Status: t.Status, Reason: t.Reason})
	}
	return out
}

func summarize(pods []*corev1.Pod, entries []collapse.Entry) Summary {
	s := Summary{Total: len(pods)}
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
