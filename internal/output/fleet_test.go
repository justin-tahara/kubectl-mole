package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/justin-tahara/kubectl-mole/internal/collapse"
)

func fleetInput() FleetInput {
	return FleetInput{
		Namespace: "",
		Selector:  "part-of=platform",
		Elapsed:   94 * time.Second,
		Targets: []FleetTarget{
			{Kind: "Deployment", Name: "web", Namespace: "a", Status: StatusSettled,
				Reason: "healthy for 15s", Pods: []*corev1.Pod{pod("web-1", true)}},
			{Kind: "Deployment", Name: "api", Namespace: "b", Status: StatusFailed,
				Reason: "pod api-x: container main in CrashLoopBackOff", Pods: []*corev1.Pod{pod("api-x", false)}},
			{Kind: "StatefulSet", Name: "db", Namespace: "b", Status: StatusProgressing,
				Reason: "still progressing: waiting for pods", Pods: []*corev1.Pod{pod("db-0", false)}},
			{Kind: "Deployment", Name: "api", Namespace: "c", Status: StatusFailed,
				Reason: "pod api-y: container main in CrashLoopBackOff", Pods: []*corev1.Pod{pod("api-y", false)}},
		},
		Failures: []collapse.Entry{{
			Signature: "CrashLoopBackOff",
			Cause:     "container main is crash-looping (last exit code 7)",
			Chain:     []string{"Deployment/api", "ReplicaSet/api-7f9c", "Pod/api-x"},
			Affected:  2,
			Examples:  []string{"b/api-x", "c/api-y"},
			Pods:      []string{"b/api-x", "c/api-y"},
		}},
	}
}

func TestBuildFleetWorstOfFleet(t *testing.T) {
	v := BuildFleet(fleetInput())
	if v.Status != StatusFailed || v.ExitCode() != ExitFailed {
		t.Fatalf("one failed target must make the fleet failed, got %q exit %d", v.Status, v.ExitCode())
	}

	in := fleetInput()
	for i := range in.Targets {
		if in.Targets[i].Status == StatusFailed {
			in.Targets[i].Status = StatusSettled
		}
	}
	if v := BuildFleet(in); v.Status != StatusProgressing || v.ExitCode() != ExitProgressing {
		t.Fatalf("progressing outranks settled, got %q exit %d", v.Status, v.ExitCode())
	}
	for i := range in.Targets {
		in.Targets[i].Status = StatusSettled
	}
	if v := BuildFleet(in); v.Status != StatusSettled || v.ExitCode() != ExitSettled {
		t.Fatalf("all settled must be settled, got %q exit %d", v.Status, v.ExitCode())
	}
}

func TestBuildFleetCountsAndScope(t *testing.T) {
	v := BuildFleet(fleetInput())
	if v.Target != "workloads" || v.Namespace != "*" || v.Selector != "part-of=platform" {
		t.Fatalf("scope: target=%q namespace=%q selector=%q", v.Target, v.Namespace, v.Selector)
	}
	want := FleetCounts{Targets: 4, Settled: 1, Failed: 2, Progressing: 1, Namespaces: 3}
	if v.Fleet == nil || *v.Fleet != want {
		t.Fatalf("fleet counts %+v, want %+v", v.Fleet, want)
	}
	if v.Reason != "2 of 4 workloads failed" {
		t.Fatalf("reason %q", v.Reason)
	}
	if got, want := v.Summary, (Summary{Total: 4, Ready: 1, Failed: 2}); got != want {
		t.Fatalf("summary %+v, want %+v", got, want)
	}
}

// TestBuildFleetNamespaceGrouping: only namespaces with non-settled targets
// appear, each carrying its worst status, in fleet order.
func TestBuildFleetNamespaceGrouping(t *testing.T) {
	v := BuildFleet(fleetInput())
	if len(v.Namespaces) != 2 {
		t.Fatalf("settled namespaces must not be enumerated, got %+v", v.Namespaces)
	}
	b, c := v.Namespaces[0], v.Namespaces[1]
	if b.Namespace != "b" || b.Status != StatusFailed || len(b.Targets) != 2 {
		t.Fatalf("namespace b: %+v", b)
	}
	if b.Targets[0].Target != "Deployment/api" || b.Targets[1].Target != "StatefulSet/db" {
		t.Fatalf("namespace b target order: %+v", b.Targets)
	}
	if c.Namespace != "c" || c.Status != StatusFailed || len(c.Targets) != 1 {
		t.Fatalf("namespace c: %+v", c)
	}
}

func TestBuildFleetDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := WriteJSON(&a, BuildFleet(fleetInput())); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&b, BuildFleet(fleetInput())); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("same input must produce byte-identical fleet output")
	}
}

// TestSingleTargetOmitsFleetFields pins the additive-schema promise: a
// single-target verdict's JSON has no selector, fleet, or namespaces keys.
func TestSingleTargetOmitsFleetFields(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, Build(failedInput())); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"selector", "fleet", "namespaces"} {
		if _, present := m[key]; present {
			t.Fatalf("single-target JSON must omit top-level %q:\n%s", key, buf.String())
		}
	}
	tr, ok := m["truncated"].(map[string]any)
	if !ok || tr["namespaces"] != float64(0) {
		t.Fatalf("truncated.namespaces must stay present (always-emitted contract), got %v", m["truncated"])
	}
}

func TestFleetErrorVerdicts(t *testing.T) {
	v := NoMatchFleet("", "app=absent", `no workloads match selector "app=absent" in any namespace`)
	if v.Status != StatusNoMatch || v.ExitCode() != ExitNoMatch {
		t.Fatalf("status %q exit %d", v.Status, v.ExitCode())
	}
	if v.Target != "workloads" || v.Namespace != "*" || v.Selector != "app=absent" {
		t.Fatalf("scope: %q %q %q", v.Target, v.Namespace, v.Selector)
	}

	p := PermissionDeniedFleet("prod", "", "cannot list pods in namespace prod")
	if p.Status != StatusPermissionDenied || p.ExitCode() != ExitPermission || p.Namespace != "prod" {
		t.Fatalf("permission fleet verdict: %+v", p)
	}
}

func TestFleetTextRendering(t *testing.T) {
	var buf bytes.Buffer
	WriteText(&buf, BuildFleet(fleetInput()))
	text := buf.String()
	for _, want := range []string{
		"workloads (all namespaces, selector part-of=platform): failed",
		"targets: 1/4 settled, 2 failed, 1 progressing (3 namespaces)",
		"namespaces:",
		"  b: failed",
		"    Deployment/api: failed (pod api-x: container main in CrashLoopBackOff)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output missing %q:\n%s", want, text)
		}
	}
}
