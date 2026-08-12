package settle

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

func wedgedOpts() Options {
	return Options{Timeout: time.Minute, StableFor: 15 * time.Second, WedgedFor: 30 * time.Second}
}

// mkwedgedpod is a pod with a container in a terminal waiting state.
func mkwedgedpod(name, uid, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "main",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "x"}},
			}},
		},
	}
}

func mkfailedpod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
}

func TestWedgedForFailsEarly(t *testing.T) {
	tr := newTracker(wedgedOpts())
	s := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkwedgedpod("a", "u1", "ImagePullBackOff")}, 0)
	for i, now := range []time.Time{t0, t0.Add(10 * time.Second), t0.Add(20 * time.Second)} {
		if out, done := tr.observe(now, s); done {
			t.Fatalf("tick %d: failed before the wedged window filled: outcome=%s", i, out)
		}
	}
	out, done := tr.observe(t0.Add(30*time.Second), s)
	if !done || out != OutcomeFailed {
		t.Fatalf("want early failure at 30s wedged, got done=%v outcome=%s", done, out)
	}
	if !strings.Contains(tr.lastReason, "ImagePullBackOff") || !strings.Contains(tr.lastReason, "wedged for 30s") {
		t.Fatalf("reason should name the wedge state and window, got %q", tr.lastReason)
	}
}

// The crash-backoff cycle flickers between the waiting reason and a brief
// running attempt. Gap ticks add nothing but must not reset the clock.
func TestWedgedForSurvivesCrashGaps(t *testing.T) {
	tr := newTracker(wedgedOpts())
	wedged := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkwedgedpod("a", "u1", "CrashLoopBackOff")}, 0)
	attempt := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkpod("a", "u1", false, 1)}, 0)

	ticks := []struct {
		at   time.Duration
		snap snapshot
	}{
		{0, wedged},                 // clock opens
		{10 * time.Second, wedged},  // +10 = 10
		{15 * time.Second, attempt}, // gap: no add, no reset
		{25 * time.Second, wedged},  // +10 = 20
		{30 * time.Second, attempt}, // gap again
	}
	for i, tick := range ticks {
		if out, done := tr.observe(t0.Add(tick.at), tick.snap); done {
			t.Fatalf("tick %d: done too early: outcome=%s", i, out)
		}
	}
	out, done := tr.observe(t0.Add(40*time.Second), wedged) // +10 = 30
	if !done || out != OutcomeFailed {
		t.Fatalf("want failure once cumulative wedge reaches 30s, got done=%v outcome=%s", done, out)
	}
}

// A pod that becomes Ready genuinely recovered: its wedged clock resets and
// patience starts over.
func TestWedgedForResetsOnReady(t *testing.T) {
	tr := newTracker(wedgedOpts())
	wedged := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkwedgedpod("a", "u1", "ImagePullBackOff")}, 0)
	ready := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 0)

	tr.observe(t0, wedged)
	tr.observe(t0.Add(10*time.Second), wedged) // 10
	tr.observe(t0.Add(20*time.Second), wedged) // 20
	tr.observe(t0.Add(25*time.Second), ready)  // recovery: clock wiped
	// A fresh wedge only holds 25s of the 30s window: must not fire.
	tr.observe(t0.Add(35*time.Second), wedged) // 10
	tr.observe(t0.Add(45*time.Second), wedged) // 20
	if out, done := tr.observe(t0.Add(50*time.Second), wedged); done { // 25
		t.Fatalf("failed 25s after a genuine recovery: outcome=%s", out)
	}
	out, done := tr.observe(t0.Add(55*time.Second), wedged) // 30
	if !done || out != OutcomeFailed {
		t.Fatalf("want failure 30s after the recovery was undone, got done=%v outcome=%s", done, out)
	}
}

func TestWedgedForZeroWaitsForTimeout(t *testing.T) {
	tr := newTracker(opts()) // WedgedFor unset = disabled
	s := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkwedgedpod("a", "u1", "ImagePullBackOff")}, 0)
	for _, at := range []time.Duration{0, 2 * time.Minute, 10 * time.Minute} {
		if out, done := tr.observe(t0.Add(at), s); done {
			t.Fatalf("disabled window must never end the watch: outcome=%s", out)
		}
	}
	if out, _ := tr.timeoutVerdict(); out != OutcomeFailed {
		t.Fatalf("timeout classification must still fail the wedge, got %s", out)
	}
}

// Job retry pods land in phase Failed routinely — that is progress, not a
// wedge. A terminal waiting reason still counts for Jobs.
func TestWedgedForCompletionTerminalTaxonomy(t *testing.T) {
	retrying := snapshot{
		found: true, generation: 1, observedGeneration: 1,
		kstatus:            kstatusResult{Status: status.InProgressStatus, Message: "job in progress"},
		completionTerminal: true,
		currentPods:        []*corev1.Pod{mkfailedpod("retry-1", "u1")},
	}
	tr := newTracker(wedgedOpts())
	for _, at := range []time.Duration{0, 20 * time.Second, 40 * time.Second, 2 * time.Minute} {
		if out, done := tr.observe(t0.Add(at), retrying); done {
			t.Fatalf("phase-Failed retry pod must not wedge a Job: outcome=%s", out)
		}
	}

	wedged := retrying
	wedged.currentPods = []*corev1.Pod{mkwedgedpod("pull", "u2", "ImagePullBackOff")}
	tr = newTracker(wedgedOpts())
	tr.observe(t0, wedged)
	tr.observe(t0.Add(15*time.Second), wedged)
	out, done := tr.observe(t0.Add(30*time.Second), wedged)
	if !done || out != OutcomeFailed {
		t.Fatalf("a Job pod wedged in ImagePullBackOff must fail early, got done=%v outcome=%s", done, out)
	}
}

// A phase-Failed pod on a readiness-settled workload wedges, matching the
// timeout verdict for the same state.
func TestWedgedForPhaseFailedOnWorkload(t *testing.T) {
	tr := newTracker(wedgedOpts())
	s := mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkfailedpod("a", "u1")}, 0)
	tr.observe(t0, s)
	tr.observe(t0.Add(15*time.Second), s)
	out, done := tr.observe(t0.Add(30*time.Second), s)
	if !done || out != OutcomeFailed {
		t.Fatalf("want early failure on wedged phase-Failed pod, got done=%v outcome=%s", done, out)
	}
	if !strings.Contains(tr.lastReason, "phase Failed") {
		t.Fatalf("reason should name phase Failed, got %q", tr.lastReason)
	}
}
