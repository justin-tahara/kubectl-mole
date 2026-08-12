package budget

import (
	"strings"
	"testing"

	"github.com/justin-tahara/kubectl-mole/internal/output"
)

func failure(name string, evidence ...string) output.Failure {
	f := output.Failure{
		Signature: "CrashLoopBackOff",
		Cause:     "container main is crash-looping (last exit code 7)",
		Chain:     "Deployment/api → ReplicaSet/api-7f9c → Pod/" + name,
		Affected:  1,
		Examples:  []string{"prod/" + name},
		Evidence:  []output.Evidence{},
	}
	for _, text := range evidence {
		f.Evidence = append(f.Evidence, output.Evidence{Source: "log", Untrusted: true, Text: text})
	}
	return f
}

func verdict(failures ...output.Failure) output.Verdict {
	v := output.Verdict{
		SchemaVersion: "1",
		Status:        output.StatusFailed,
		Target:        "Deployment/api",
		Namespace:     "prod",
		Reason:        "pods failing",
		Elapsed:       "94s",
		Summary:       output.Summary{Total: 3, Ready: 0, Failed: 3},
		Failures:      failures,
		Degraded:      []string{},
	}
	v.ContentHash = output.Hash(v)
	return v
}

func TestZeroBudgetIsUnlimited(t *testing.T) {
	v := verdict(failure("a", strings.Repeat("x", 4000)))
	got := Apply(v, 0)
	if got.Truncated.Failures != 0 || got.Truncated.Evidence != 0 || len(got.Failures[0].Evidence) != 1 {
		t.Fatalf("budget 0 must not trim: %+v", got.Truncated)
	}
}

func TestUnderBudgetUntouched(t *testing.T) {
	v := verdict(failure("a", "short"))
	got := Apply(v, Tokens(v)+10)
	if got.ContentHash != v.ContentHash {
		t.Fatal("a verdict under budget must pass through unchanged")
	}
}

func TestEvidenceDroppedBeforeFailures(t *testing.T) {
	v := verdict(
		failure("a", strings.Repeat("a", 2000)),
		failure("b", strings.Repeat("b", 2000)),
	)
	bare := verdict(failure("a"), failure("b"))
	got := Apply(v, Tokens(bare)+20)
	if len(got.Failures) != 2 {
		t.Fatalf("both failure entries must survive, got %d", len(got.Failures))
	}
	if got.Truncated.Evidence != 2 || got.Truncated.Failures != 0 {
		t.Fatalf("want 2 evidence items dropped and no failures, got %+v", got.Truncated)
	}
	if got.ContentHash == v.ContentHash || got.ContentHash != output.Hash(got) {
		t.Fatal("trimmed verdict must be rehashed")
	}
}

func TestRoundRobinEvidence(t *testing.T) {
	first := strings.Repeat("1", 400)
	v := verdict(
		failure("a", "A-FIRST-"+first, "A-SECOND-"+strings.Repeat("2", 400)),
		failure("b", "B-FIRST-"+first, "B-SECOND-"+strings.Repeat("2", 400)),
	)
	// Budget sized to hold exactly each entry's first item.
	target := verdict(failure("a", "A-FIRST-"+first), failure("b", "B-FIRST-"+first))
	got := Apply(v, Tokens(target)+5)
	for i, want := range []string{"A-FIRST", "B-FIRST"} {
		evs := got.Failures[i].Evidence
		if len(evs) != 1 || !strings.HasPrefix(evs[0].Text, want) {
			t.Fatalf("failure %d must keep its first evidence item only, got %+v", i, evs)
		}
	}
	if got.Truncated.Evidence != 2 {
		t.Fatalf("two second-round items dropped, got %+v", got.Truncated)
	}
}

func TestFailuresDroppedFromTheEnd(t *testing.T) {
	v := verdict(failure("a"), failure("b"), failure("c"))
	got := Apply(v, Tokens(verdict(failure("a")))+5)
	if len(got.Failures) != 1 || !strings.HasSuffix(got.Failures[0].Chain, "Pod/a") {
		t.Fatalf("want only the first entry kept, got %+v", got.Failures)
	}
	if got.Truncated.Failures != 2 || got.Truncated.Evidence != 0 {
		t.Fatalf("dropped entries count as failures, not evidence: %+v", got.Truncated)
	}
}

func TestSkeletonAlwaysEmitted(t *testing.T) {
	v := verdict(failure("a", "evidence"), failure("b"))
	got := Apply(v, 1)
	if got.Status != output.StatusFailed || got.Summary.Failed != 3 {
		t.Fatal("tier 0 must survive any budget")
	}
	if len(got.Failures) != 0 || got.Truncated.Failures != 2 {
		t.Fatalf("everything else dropped and counted, got %+v", got.Truncated)
	}
}

func TestApplyIsDeterministic(t *testing.T) {
	v := verdict(
		failure("a", strings.Repeat("a", 900), "small-a"),
		failure("b", strings.Repeat("b", 900)),
	)
	one := Apply(v, 300)
	two := Apply(v, 300)
	if output.Hash(one) != output.Hash(two) {
		t.Fatal("same input and budget must produce identical output")
	}
}

func fleetVerdict(namespaces int, failures ...output.Failure) output.Verdict {
	v := verdict(failures...)
	v.Target = "workloads"
	v.Namespace = "*"
	v.Selector = "part-of=platform"
	v.Fleet = &output.FleetCounts{Targets: namespaces, Failed: namespaces, Namespaces: namespaces}
	for i := 0; i < namespaces; i++ {
		name := "tenant-" + strings.Repeat("z", 20) + string(rune('a'+i))
		v.Namespaces = append(v.Namespaces, output.NamespaceVerdict{
			Namespace: name,
			Status:    output.StatusFailed,
			Targets: []output.TargetVerdict{{
				Target: "Deployment/api", Status: output.StatusFailed,
				Reason: "pod api-x: container main in CrashLoopBackOff",
			}},
		})
	}
	v.ContentHash = output.Hash(v)
	return v
}

// TestNamespacesDroppedBeforeFailures: when the budget cannot hold both, the
// collapsed causes survive and the per-namespace enumeration goes first.
func TestNamespacesDroppedBeforeFailures(t *testing.T) {
	v := fleetVerdict(8, failure("a"))

	bare := fleetVerdict(0, failure("a"))
	got := Apply(v, Tokens(bare)+10)
	if len(got.Failures) != 1 {
		t.Fatalf("failure entry must survive namespace trimming, got %d failures", len(got.Failures))
	}
	if got.Truncated.Namespaces == 0 || len(got.Namespaces) == len(v.Namespaces) {
		t.Fatalf("namespaces must be dropped and counted: kept %d, truncated %+v", len(got.Namespaces), got.Truncated)
	}
	if len(got.Namespaces)+got.Truncated.Namespaces != len(v.Namespaces) {
		t.Fatalf("accounting mismatch: kept %d + truncated %d != %d", len(got.Namespaces), got.Truncated.Namespaces, len(v.Namespaces))
	}
}

// TestEvidenceDroppedBeforeNamespaces: stripping evidence alone can satisfy
// the budget; the namespace entries stay.
func TestEvidenceDroppedBeforeNamespaces(t *testing.T) {
	v := fleetVerdict(3, failure("a", strings.Repeat("x", 4000)))
	stripped := fleetVerdict(3, failure("a"))
	got := Apply(v, Tokens(stripped)+10)
	if len(got.Namespaces) != 3 || got.Truncated.Namespaces != 0 {
		t.Fatalf("namespaces must survive when dropping evidence suffices: %+v", got.Truncated)
	}
	if got.Truncated.Evidence != 1 {
		t.Fatalf("evidence must be the first tier dropped, truncated %+v", got.Truncated)
	}
}

// TestFleetSkeletonAlwaysEmitted: even budget 1 keeps status and counts.
func TestFleetSkeletonAlwaysEmitted(t *testing.T) {
	got := Apply(fleetVerdict(5, failure("a")), 1)
	if got.Status != output.StatusFailed || got.Fleet == nil || got.Fleet.Targets != 5 {
		t.Fatalf("skeleton must survive: %+v", got)
	}
	if len(got.Namespaces) != 0 || len(got.Failures) != 0 {
		t.Fatalf("budget 1 keeps only the skeleton, got %d ns %d failures", len(got.Namespaces), len(got.Failures))
	}
	if got.Truncated.Namespaces != 5 || got.Truncated.Failures != 1 {
		t.Fatalf("drops must be counted: %+v", got.Truncated)
	}
}
