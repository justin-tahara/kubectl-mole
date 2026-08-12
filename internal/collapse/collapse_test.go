package collapse

import (
	"testing"

	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

func podFinding(ns, pod, cause string) signatures.Finding {
	return signatures.Finding{
		Signature: "CrashLoopBackOff",
		Cause:     cause,
		Chain:     []string{"Deployment/api", "ReplicaSet/api-7f9c", "Pod/" + pod},
		Pod:       pod,
		Namespace: ns,
	}
}

func TestIdenticalCausesCollapse(t *testing.T) {
	findings := []signatures.Finding{
		podFinding("prod", "api-7f9c-a", "container main is crash-looping (last exit code 7)"),
		podFinding("prod", "api-7f9c-b", "container main is crash-looping (last exit code 7)"),
		podFinding("prod", "api-7f9c-c", "container main is crash-looping (last exit code 7)"),
	}
	entries := Collapse(findings)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Affected != 3 || len(e.Pods) != 3 {
		t.Fatalf("affected %d pods %v, want 3/3", e.Affected, e.Pods)
	}
	want := []string{"prod/api-7f9c-a", "prod/api-7f9c-b", "prod/api-7f9c-c"}
	for i, ref := range want {
		if e.Examples[i] != ref {
			t.Fatalf("examples %v, want %v", e.Examples, want)
		}
	}
	if e.Chain[2] != "Pod/api-7f9c-a" {
		t.Fatalf("chain should come from the first finding, got %v", e.Chain)
	}
}

// TestCrossNamespaceCollapse is the fan-out case the collapse layer exists
// for: the same cause in many namespaces is one entry, with anchors kept
// distinct per namespace even when pod names repeat.
func TestCrossNamespaceCollapse(t *testing.T) {
	findings := []signatures.Finding{
		podFinding("tenant-a", "api-7f9c-x", "container main is crash-looping (last exit code 7)"),
		podFinding("tenant-b", "api-7f9c-x", "container main is crash-looping (last exit code 7)"),
		podFinding("tenant-c", "api-7f9c-x", "container main is crash-looping (last exit code 7)"),
		podFinding("tenant-d", "api-7f9c-x", "container main is crash-looping (last exit code 7)"),
	}
	entries := Collapse(findings)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry across namespaces, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Affected != 4 || len(e.Pods) != 4 {
		t.Fatalf("same pod name in 4 namespaces must count 4 anchors, got affected=%d pods=%v", e.Affected, e.Pods)
	}
	want := []string{"tenant-a/api-7f9c-x", "tenant-b/api-7f9c-x", "tenant-c/api-7f9c-x"}
	for i, ref := range want {
		if e.Examples[i] != ref {
			t.Fatalf("examples %v, want %v", e.Examples, want)
		}
	}
}

func TestExamplesCapped(t *testing.T) {
	var findings []signatures.Finding
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		findings = append(findings, podFinding("prod", "api-"+p, "same cause"))
	}
	e := Collapse(findings)[0]
	if e.Affected != 5 || len(e.Examples) != 3 {
		t.Fatalf("affected %d examples %v, want 5 with 3 examples", e.Affected, e.Examples)
	}
}

func TestDifferentCausesStaySeparate(t *testing.T) {
	findings := []signatures.Finding{
		podFinding("prod", "api-a", "container main is crash-looping (last exit code 7)"),
		podFinding("prod", "api-b", "container main is crash-looping (last exit code 1)"),
	}
	entries := Collapse(findings)
	if len(entries) != 2 {
		t.Fatalf("different causes must not merge, got %+v", entries)
	}
	for _, e := range entries {
		if e.Affected != 1 {
			t.Fatalf("each cause anchors one pod, got %+v", e)
		}
	}
}

func TestWorkloadAnchorCountsOnce(t *testing.T) {
	wf := signatures.Finding{
		Signature: "QuotaExceeded",
		Cause:     `pod creation blocked by quota "no-pods" (requested pods=1)`,
		Chain:     []string{"Deployment/api"},
		Namespace: "prod",
	}
	entries := Collapse([]signatures.Finding{wf, wf})
	if len(entries) != 1 || entries[0].Affected != 1 {
		t.Fatalf("same workload anchor must count once, got %+v", entries)
	}
	if entries[0].Examples[0] != "prod/api" {
		t.Fatalf("workload example should be namespace/name, got %v", entries[0].Examples)
	}
	if len(entries[0].Pods) != 0 {
		t.Fatalf("workload-level entry anchors no pods, got %v", entries[0].Pods)
	}
}
