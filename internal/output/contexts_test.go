package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func contextsInput() FleetInput {
	return FleetInput{
		Target:    "Deployment/api",
		Namespace: "prod",
		Contexts:  []ContextVerdict{{Context: "east"}, {Context: "west"}},
		Targets: []FleetTarget{
			{Kind: "Deployment", Name: "api", Namespace: "prod", Context: "east",
				Status: StatusSettled, Reason: "healthy for 15s"},
			{Kind: "Deployment", Name: "api", Namespace: "prod", Context: "west",
				Status: StatusFailed, Reason: "pod api-x: container main in CrashLoopBackOff"},
		},
	}
}

// The merge ordering is the multi-cluster contract: the worst context wins
// the verdict, and an unverified cluster (permission denied, no match)
// outranks progressing so "keep waiting" can never mask it.
func TestBuildContextsWorstOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		want string
		exit int
	}{
		{StatusSettled, StatusSettled, StatusSettled, ExitSettled},
		{StatusSettled, StatusProgressing, StatusProgressing, ExitProgressing},
		{StatusProgressing, StatusNoMatch, StatusNoMatch, ExitNoMatch},
		{StatusNoMatch, StatusPermissionDenied, StatusPermissionDenied, ExitPermission},
		{StatusPermissionDenied, StatusFailed, StatusFailed, ExitFailed},
		{StatusProgressing, StatusFailed, StatusFailed, ExitFailed},
	}
	for _, c := range cases {
		in := FleetInput{Contexts: []ContextVerdict{
			{Context: "a", Status: c.a, Reason: "r"},
			{Context: "b", Status: c.b, Reason: "r"},
		}}
		v := BuildFleet(in)
		if v.Status != c.want || v.ExitCode() != c.exit {
			t.Fatalf("contexts %q+%q: got status %q exit %d, want %q exit %d",
				c.a, c.b, v.Status, v.ExitCode(), c.want, c.exit)
		}
	}
}

func TestBuildContextsFillsEntries(t *testing.T) {
	v := BuildFleet(contextsInput())
	if v.Target != "Deployment/api" {
		t.Fatalf("named multi-context target lost: %q", v.Target)
	}
	if v.Status != StatusFailed || v.Reason != "1 of 2 contexts failed" {
		t.Fatalf("got status %q reason %q", v.Status, v.Reason)
	}
	want := []ContextVerdict{
		{Context: "east", Status: StatusSettled, Reason: "healthy for 15s"},
		{Context: "west", Status: StatusFailed, Reason: "pod api-x: container main in CrashLoopBackOff"},
	}
	if len(v.Contexts) != 2 || v.Contexts[0] != want[0] || v.Contexts[1] != want[1] {
		t.Fatalf("rollup entries wrong: %+v", v.Contexts)
	}
	if v.Fleet == nil || v.Fleet.Contexts != 2 || v.Fleet.Targets != 2 || v.Fleet.Settled != 1 || v.Fleet.Failed != 1 {
		t.Fatalf("fleet counts wrong: %+v", v.Fleet)
	}
	if v.Fleet.Namespaces != 2 {
		t.Fatalf("prod in two clusters is two namespaces, counted %d", v.Fleet.Namespaces)
	}
	if len(v.Namespaces) != 1 || v.Namespaces[0].Context != "west" || v.Namespaces[0].Namespace != "prod" {
		t.Fatalf("namespace entries wrong: %+v", v.Namespaces)
	}
}

// The same namespace name in two clusters is two different namespaces: the
// entries must not merge.
func TestBuildContextsNamespaceGroupingPerContext(t *testing.T) {
	in := FleetInput{
		Contexts: []ContextVerdict{{Context: "east"}, {Context: "west"}},
		Targets: []FleetTarget{
			{Kind: "Deployment", Name: "api", Namespace: "prod", Context: "east", Status: StatusFailed, Reason: "r"},
			{Kind: "Deployment", Name: "api", Namespace: "prod", Context: "west", Status: StatusFailed, Reason: "r"},
		},
	}
	v := BuildFleet(in)
	if len(v.Namespaces) != 2 {
		t.Fatalf("want one namespace entry per context, got %+v", v.Namespaces)
	}
	if v.Namespaces[0].Context == v.Namespaces[1].Context {
		t.Fatalf("entries lost their contexts: %+v", v.Namespaces)
	}
}

// An entry the caller left unfilled with no targets behind it must never be
// judged settled — that would report a cluster nobody watched as healthy.
func TestBuildContextsUnwatchedEntryNeverSettles(t *testing.T) {
	in := FleetInput{Contexts: []ContextVerdict{{Context: "ghost"}}}
	v := BuildFleet(in)
	if v.Contexts[0].Status != StatusNoMatch {
		t.Fatalf("unwatched context judged %q", v.Contexts[0].Status)
	}
	if v.Status == StatusSettled || v.ExitCode() == ExitSettled {
		t.Fatalf("verdict settled over an unwatched context")
	}
}

// Single-cluster verdicts must stay byte-identical: no contexts key anywhere.
func TestSingleClusterOmitsContextFields(t *testing.T) {
	v := BuildFleet(fleetInput())
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"context`) {
		t.Fatalf("single-cluster verdict leaks context fields: %s", b)
	}
}

func TestContextsTextRendering(t *testing.T) {
	v := BuildFleet(contextsInput())
	var buf bytes.Buffer
	WriteText(&buf, v, nil)
	text := buf.String()
	for _, want := range []string{
		"contexts east,west; namespace prod",
		"contexts:",
		"east: settled (healthy for 15s)",
		"west: failed",
		"prod (west): failed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output missing %q:\n%s", want, text)
		}
	}
}

func TestBuildContextsDeterministic(t *testing.T) {
	a, b := BuildFleet(contextsInput()), BuildFleet(contextsInput())
	if a.ContentHash != b.ContentHash {
		t.Fatalf("same input, different hashes: %s vs %s", a.ContentHash, b.ContentHash)
	}
}
