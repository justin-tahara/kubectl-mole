package output

import (
	"testing"

	"k8s.io/utils/ptr"
)

func settledWithAdvisory(finishedAt, reason string, exit int32) Verdict {
	v := Verdict{
		SchemaVersion: "1",
		Status:        StatusSettled,
		Target:        "Deployment/worker",
		Namespace:     "app",
		Summary:       Summary{Total: 1, Ready: 1},
		Advisories: []Advisory{{
			Kind:                 "recent-restarts",
			TerminationsInWindow: 1,
			Window:               "24h",
			LastExitCode:         ptr.To(exit),
			LastReason:           reason,
			LastFinishedAt:       finishedAt,
			Text:                 "containers restarted recently",
		}},
	}
	v.ContentHash = Hash(v)
	return v
}

// The design catch that makes delta mode diff advisories on their own
// (issue #46): a container killed and recovered between runs changes ONLY
// the advisory — Hash excludes advisories, so the settle-state hash reports
// "nothing moved" at the exact moment the watched-for event happened.
func TestDiffSeesKillAndRecoverThroughIdenticalHash(t *testing.T) {
	prev := settledWithAdvisory("2026-08-13T10:00:00Z", "OOMKilled", 137)
	cur := settledWithAdvisory("2026-08-13T11:30:00Z", "OOMKilled", 137)
	if prev.ContentHash != cur.ContentHash {
		t.Fatal("test premise broken: the settle-state hashes must be identical")
	}

	d := Diff(prev, cur)
	if !d.Changed || len(d.Transitions) != 1 {
		t.Fatalf("the new kill must be a transition: %+v", d)
	}
	tr := d.Transitions[0]
	if tr.Kind != TransitionNewTermination || tr.LastReason != "OOMKilled" || *tr.LastExitCode != 137 {
		t.Fatalf("transition must carry the new termination's facts: %+v", tr)
	}

	cur.Delta = d
	if got := cur.ExitCode(); got != ExitChanged {
		t.Fatalf("changed-but-settled must exit %d, got %d", ExitChanged, got)
	}
}

// Nothing moved — including a standing failure: delta mode exists to stop a
// scheduled sweep from re-reporting state it already reported last run.
func TestDiffUnchangedIsQuiet(t *testing.T) {
	prev := settledWithAdvisory("2026-08-13T10:00:00Z", "Error", 7)
	cur := settledWithAdvisory("2026-08-13T10:00:00Z", "Error", 7)
	d := Diff(prev, cur)
	if d.Changed || len(d.Transitions) != 0 {
		t.Fatalf("identical verdicts must not report transitions: %+v", d)
	}
	cur.Delta = d
	if got := cur.ExitCode(); got != ExitSettled {
		t.Fatalf("unchanged must exit 0, got %d", got)
	}

	failed := Verdict{SchemaVersion: "1", Status: StatusFailed, Target: "Deployment/worker", Namespace: "app"}
	failed.ContentHash = Hash(failed)
	d = Diff(failed, failed)
	failedNow := failed
	failedNow.Delta = d
	if d.Changed || failedNow.ExitCode() != ExitSettled {
		t.Fatalf("a standing failure is not news: %+v exit %d", d, failedNow.ExitCode())
	}
	if failed.ExitCode() != ExitFailed {
		t.Fatal("without delta the same verdict must keep exit 1")
	}
}

func fleetVerdict(status string, entries ...NamespaceVerdict) Verdict {
	v := Verdict{
		SchemaVersion: "1",
		Status:        status,
		Target:        "workloads",
		Namespace:     "*",
		Fleet:         &FleetCounts{Targets: 10},
		Namespaces:    entries,
	}
	v.ContentHash = Hash(v)
	return v
}

// Fleet verdicts count settled targets instead of listing them, so the diff
// reads list membership: absent from a checked verdict's failing list means
// settled, on both sides of the comparison.
func TestDiffFleetFailingList(t *testing.T) {
	prev := fleetVerdict(StatusFailed, NamespaceVerdict{
		Namespace: "app", Status: StatusFailed,
		Targets: []TargetVerdict{{Target: "Deployment/broken", Status: StatusFailed, Reason: "crash-looping"}},
	})
	cur := fleetVerdict(StatusFailed, NamespaceVerdict{
		Namespace: "shop", Status: StatusFailed,
		Targets: []TargetVerdict{{Target: "Deployment/checkout", Status: StatusFailed, Reason: "crash-looping"}},
	})

	d := Diff(prev, cur)
	if len(d.Transitions) != 2 {
		t.Fatalf("want a recovery and a new failure, got %+v", d.Transitions)
	}
	byTarget := map[string]Transition{}
	for _, tr := range d.Transitions {
		byTarget[tr.Target] = tr
	}
	if tr := byTarget["Deployment/broken"]; tr.From != StatusFailed || tr.To != StatusSettled {
		t.Fatalf("leaving the failing list must read as settled: %+v", tr)
	}
	if tr := byTarget["Deployment/checkout"]; tr.From != StatusSettled || tr.To != StatusFailed {
		t.Fatalf("joining the failing list must read as newly failed: %+v", tr)
	}
}

// A structural status means the run enumerated nothing; the diff must not
// invent recoveries from an empty list it never checked.
func TestDiffStructuralCurrentSuppressesInference(t *testing.T) {
	prev := fleetVerdict(StatusFailed, NamespaceVerdict{
		Namespace: "app", Status: StatusFailed,
		Targets: []TargetVerdict{{Target: "Deployment/broken", Status: StatusFailed, Reason: "crash-looping"}},
	})
	cur := fleetVerdict(StatusNoMatch)
	cur.Reason = "no workloads matched"

	d := Diff(prev, cur)
	for _, tr := range d.Transitions {
		if tr.To == StatusSettled {
			t.Fatalf("an unchecked verdict must not report recoveries: %+v", d.Transitions)
		}
	}
	// The whole-verdict flip is still a transition, and exit 4 is preserved.
	if !d.Changed {
		t.Fatal("failed -> no_resources_matched is a transition")
	}
	cur.Delta = d
	if got := cur.ExitCode(); got != ExitNoMatch {
		t.Fatalf("structural codes survive delta mode, got %d", got)
	}
}

// Context rollup changes are transitions — a fleet cron over --contexts
// wants "cluster unreachable" as the event, not per-target noise.
func TestDiffContextTransition(t *testing.T) {
	prev := fleetVerdict(StatusSettled)
	prev.Contexts = []ContextVerdict{{Context: "east", Status: StatusSettled, Reason: "ok"}, {Context: "west", Status: StatusSettled, Reason: "ok"}}
	prev.ContentHash = Hash(prev)
	cur := fleetVerdict(StatusFailed)
	cur.Contexts = []ContextVerdict{{Context: "east", Status: StatusSettled, Reason: "ok"}, {Context: "west", Status: StatusFailed, Reason: "watch failed: unreachable"}}
	cur.ContentHash = Hash(cur)

	d := Diff(prev, cur)
	if len(d.Transitions) != 1 || d.Transitions[0].Kind != TransitionContext || d.Transitions[0].Context != "west" {
		t.Fatalf("want exactly the west context transition, got %+v", d.Transitions)
	}
}

// Advisory lifecycle: appearing on a quiet target and aging out are both
// transitions; a vanished advisory on a now-failing target is not — that
// target's story is its status transition.
func TestDiffAdvisoryLifecycle(t *testing.T) {
	quiet := Verdict{SchemaVersion: "1", Status: StatusSettled, Target: "Deployment/worker", Namespace: "app"}
	quiet.ContentHash = Hash(quiet)
	noisy := settledWithAdvisory("2026-08-13T10:00:00Z", "OOMKilled", 137)

	d := Diff(quiet, noisy)
	if len(d.Transitions) != 1 || d.Transitions[0].Kind != TransitionAdvisoryAppeared {
		t.Fatalf("advisory appearing is a transition: %+v", d.Transitions)
	}
	d = Diff(noisy, quiet)
	if len(d.Transitions) != 1 || d.Transitions[0].Kind != TransitionAdvisoryCleared {
		t.Fatalf("advisory aging out is a transition: %+v", d.Transitions)
	}

	// Same key failing now: the advisory's disappearance is not the story.
	prev := fleetVerdict(StatusSettled)
	prev.Advisories = []Advisory{{Kind: "recent-restarts", Target: "Deployment/worker", Namespace: "app",
		LastExitCode: ptr.To(int32(137)), LastFinishedAt: "2026-08-13T10:00:00Z", Text: "restarted"}}
	prev.ContentHash = Hash(prev)
	cur := fleetVerdict(StatusFailed, NamespaceVerdict{
		Namespace: "app", Status: StatusFailed,
		Targets: []TargetVerdict{{Target: "Deployment/worker", Status: StatusFailed, Reason: "crash-looping"}},
	})
	d = Diff(prev, cur)
	for _, tr := range d.Transitions {
		if tr.Kind == TransitionAdvisoryCleared {
			t.Fatalf("no advisory-cleared on a target that now fails: %+v", d.Transitions)
		}
	}
}

// A previous verdict written before lastFinishedAt existed still diffs on
// the stable facts it has — a loop keeps working across an upgrade.
func TestDiffOldFileFallback(t *testing.T) {
	prev := settledWithAdvisory("", "Error", 7)
	same := settledWithAdvisory("", "Error", 7)
	if d := Diff(prev, same); d.Changed {
		t.Fatalf("identical facts must not fire without timestamps: %+v", d)
	}
	oom := settledWithAdvisory("2026-08-13T11:00:00Z", "OOMKilled", 137)
	d := Diff(prev, oom)
	if len(d.Transitions) != 1 || d.Transitions[0].Kind != TransitionNewTermination {
		t.Fatalf("changed facts must fire without a previous timestamp: %+v", d.Transitions)
	}
}

// A baseline run (no previous verdict) keeps the normal exit codes: the
// first run of a loop reports full state, not an empty diff.
func TestBaselineKeepsNormalExitCodes(t *testing.T) {
	failed := Verdict{SchemaVersion: "1", Status: StatusFailed,
		Delta: &Delta{Baseline: true, Transitions: []Transition{}}}
	if got := failed.ExitCode(); got != ExitFailed {
		t.Fatalf("baseline must keep exit 1, got %d", got)
	}
}
