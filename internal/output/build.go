package output

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/justin-tahara/kubectl-mole/internal/signatures"
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
	Pods   []*corev1.Pod
	Report signatures.Report
}

// Build assembles the schemaVersion "1" verdict. Ordering is inherited from
// the report (workload-level findings first, then pods sorted by name), so
// the output is deterministic.
func Build(in Input) Verdict {
	v := Verdict{
		SchemaVersion: SchemaVersion,
		Status:        in.Status,
		Target:        in.Kind + "/" + in.Name,
		Namespace:     in.Namespace,
		Reason:        in.Reason,
		Elapsed:       in.Elapsed.Round(time.Second).String(),
		Summary:       summarize(in.Pods, in.Report.Findings),
		Failures:      []Failure{},
		Degraded:      append([]string{}, in.Report.Degraded...),
	}
	for _, f := range in.Report.Findings {
		out := Failure{
			Signature: f.Signature,
			Cause:     f.Cause,
			Chain:     strings.Join(f.Chain, " → "),
			Affected:  1,
			Examples:  []string{in.Namespace + "/" + in.Name},
			Evidence:  []Evidence{},
		}
		for _, ev := range f.Evidence {
			out.Evidence = append(out.Evidence, Evidence{Source: ev.Source, Untrusted: true, Text: ev.Text})
		}
		v.Failures = append(v.Failures, out)
	}
	v.ContentHash = contentHash(v)
	return v
}

// NoMatch is the exit-4 verdict: the target matched nothing. kubectl exits 0
// on an empty match and consumers read that as success — this status exists
// to kill that false positive.
func NoMatch(kind, name, namespace, reason string) Verdict {
	return errorVerdict(kind, name, namespace, StatusNoMatch, reason)
}

// PermissionDenied is the exit-3 verdict: RBAC blocked the reads the check
// depends on. The reason names the missing verb and resource; a raw 403 never
// reaches the output.
func PermissionDenied(kind, name, namespace, reason string) Verdict {
	return errorVerdict(kind, name, namespace, StatusPermissionDenied, reason)
}

func errorVerdict(kind, name, namespace, status, reason string) Verdict {
	v := Verdict{
		SchemaVersion: SchemaVersion,
		Status:        status,
		Target:        kind + "/" + name,
		Namespace:     namespace,
		Reason:        reason,
		Elapsed:       "0s",
		Failures:      []Failure{},
		Degraded:      []string{},
	}
	v.ContentHash = contentHash(v)
	return v
}

func summarize(pods []*corev1.Pod, findings []signatures.Finding) Summary {
	s := Summary{Total: len(pods)}
	for _, p := range pods {
		if podIsReady(p) {
			s.Ready++
		}
	}
	failed := map[string]bool{}
	for _, f := range findings {
		if f.Pod != "" {
			failed[f.Pod] = true
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
