package settle

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

var t0 = time.Unix(1_000_000, 0)

func mkpod(name, uid string, ready bool, restarts int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", RestartCount: restarts}},
		},
	}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

func mksnap(gen, observed int64, st status.Status, pods []*corev1.Pod, oldPods int) snapshot {
	var old []*corev1.Pod
	for i := 0; i < oldPods; i++ {
		old = append(old, mkpod(fmt.Sprintf("old-%d", i), fmt.Sprintf("old-u%d", i), false, 0))
	}
	return snapshot{
		found:              true,
		generation:         gen,
		observedGeneration: observed,
		kstatus:            kstatusResult{Status: st, Message: "msg"},
		currentPods:        pods,
		oldPods:            old,
	}
}

func opts() Options { return Options{Timeout: time.Minute, StableFor: 15 * time.Second} }

func TestObservedGenerationLagBlocksSettle(t *testing.T) {
	tr := newTracker(opts())
	// Status looks healthy, but the controller has not seen generation 2 yet:
	// this is the previous rollout's status and must not be evaluated.
	s := mksnap(2, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 0)
	if out, done := tr.observe(t0, s); done {
		t.Fatalf("settled on stale status: outcome=%s", out)
	}
	if !strings.Contains(tr.lastReason, "generation") {
		t.Fatalf("reason should name the generation lag, got %q", tr.lastReason)
	}
}

func TestOldPodsBlockSettle(t *testing.T) {
	tr := newTracker(opts())
	s := mksnap(2, 2, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 1)
	// Hold long past the stability window: an old pod existing must always block.
	for i, now := range []time.Time{t0, t0.Add(20 * time.Second), t0.Add(40 * time.Second)} {
		if out, done := tr.observe(now, s); done {
			t.Fatalf("tick %d: settled while a previous-revision pod exists: outcome=%s", i, out)
		}
	}
	if !strings.Contains(tr.lastReason, "previous revisions") {
		t.Fatalf("reason should name old pods, got %q", tr.lastReason)
	}
}

func TestWindowCompletes(t *testing.T) {
	tr := newTracker(opts())
	s := mksnap(1, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 0)
	if _, done := tr.observe(t0, s); done {
		t.Fatal("settled before the stability window opened")
	}
	if _, done := tr.observe(t0.Add(10*time.Second), s); done {
		t.Fatal("settled 10s into a 15s stability window")
	}
	out, done := tr.observe(t0.Add(16*time.Second), s)
	if !done || out != OutcomeSettled {
		t.Fatalf("want settled after window, got done=%v outcome=%s", done, out)
	}
}

func TestRestartDuringWindowResets(t *testing.T) {
	tr := newTracker(opts())
	healthy := mksnap(1, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 0)
	restarted := mksnap(1, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 1)}, 0)

	tr.observe(t0, healthy) // window opens
	if _, done := tr.observe(t0.Add(10*time.Second), restarted); done {
		t.Fatal("settled on the tick that saw a restart")
	}
	if !strings.Contains(tr.lastReason, "restarted") {
		t.Fatalf("reason should name the restart, got %q", tr.lastReason)
	}
	// Only 10s since the reset: must not be settled yet.
	if _, done := tr.observe(t0.Add(20*time.Second), restarted); done {
		t.Fatal("settled before the restarted window completed")
	}
	// 16s since the reset: settles against the new baseline.
	out, done := tr.observe(t0.Add(26*time.Second), restarted)
	if !done || out != OutcomeSettled {
		t.Fatalf("want settled after restarted window, got done=%v outcome=%s", done, out)
	}
}

func TestPodAppearingDuringWindowResets(t *testing.T) {
	tr := newTracker(opts())
	one := mksnap(1, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 0)
	two := mksnap(1, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0), mkpod("b", "u2", true, 0)}, 0)

	tr.observe(t0, one)
	if _, done := tr.observe(t0.Add(14*time.Second), two); done {
		t.Fatal("settled on the tick where the pod set changed")
	}
	if !strings.Contains(tr.lastReason, "window restarted") {
		t.Fatalf("reason should say the window restarted, got %q", tr.lastReason)
	}
}

func TestKstatusFailedIsTerminal(t *testing.T) {
	tr := newTracker(opts())
	s := mksnap(1, 1, status.FailedStatus, nil, 0)
	out, done := tr.observe(t0, s)
	if !done || out != OutcomeFailed {
		t.Fatalf("want immediate failed on kstatus Failed, got done=%v outcome=%s", done, out)
	}
}

func TestTimeoutClassification(t *testing.T) {
	// Unready but clean pod: progressing, never failed.
	tr := newTracker(opts())
	tr.observe(t0, mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkpod("a", "u1", false, 0)}, 0))
	if out, reason := tr.timeoutVerdict(); out != OutcomeProgressing {
		t.Fatalf("clean unready pod at timeout: want progressing, got %s (%s)", out, reason)
	}

	// Terminal waiting reason: failed.
	tr = newTracker(opts())
	crash := mkpod("a", "u1", false, 3)
	crash.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	tr.observe(t0, mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{crash}, 0))
	if out, reason := tr.timeoutVerdict(); out != OutcomeFailed {
		t.Fatalf("crashlooping pod at timeout: want failed, got %s (%s)", out, reason)
	}

	// Restart observed mid-watch, pod currently between crashes: still failed.
	tr = newTracker(opts())
	tr.observe(t0, mksnap(1, 1, status.CurrentStatus, []*corev1.Pod{mkpod("a", "u1", true, 0)}, 0))
	tr.observe(t0.Add(5*time.Second), mksnap(1, 1, status.InProgressStatus, []*corev1.Pod{mkpod("a", "u1", false, 1)}, 0))
	if out, reason := tr.timeoutVerdict(); out != OutcomeFailed {
		t.Fatalf("restart observed during watch: want failed at timeout, got %s (%s)", out, reason)
	}
}

// completionSnap is a completion-terminal snapshot (Job/CronJob semantics).
func completionSnap(st status.Status, msg string, pods []*corev1.Pod) snapshot {
	return snapshot{
		found:              true,
		kstatus:            kstatusResult{Status: st, Message: msg},
		currentPods:        pods,
		completionTerminal: true,
	}
}

func TestJobCompletionSettlesImmediately(t *testing.T) {
	tr := newTracker(opts())
	// No stability window for completion: Complete cannot regress.
	out, done := tr.observe(t0, completionSnap(status.CurrentStatus, "Job Completed", nil))
	if !done || out != OutcomeSettled {
		t.Fatalf("completion must settle immediately, got done=%v out=%s", done, out)
	}
}

func TestSuspendedJobFailsImmediately(t *testing.T) {
	tr := newTracker(opts())
	s := snapshot{found: true, completionTerminal: true, terminalFailure: "job x is suspended (spec.suspend) and will not run until unsuspended"}
	out, done := tr.observe(t0, s)
	if !done || out != OutcomeFailed {
		t.Fatalf("suspension is terminal, got done=%v out=%s", done, out)
	}
	if !strings.Contains(tr.lastReason, "suspended") {
		t.Fatalf("reason should say suspended, got %q", tr.lastReason)
	}
}

// TestRetryingJobAtTimeoutIsProgressing: retry pods in phase Failed and
// restarts are how a Job under backoffLimit looks — the controller has not
// declared it failed, and neither may mole.
func TestRetryingJobAtTimeoutIsProgressing(t *testing.T) {
	tr := newTracker(opts())
	failed := mkpod("retry-1", "u1", false, 0)
	failed.Status.Phase = corev1.PodFailed
	restarted := mkpod("retry-2", "u2", false, 2)
	tr.observe(t0, completionSnap(status.InProgressStatus, "Job in progress", []*corev1.Pod{failed, restarted}))
	out, reason := tr.timeoutVerdict()
	if out != OutcomeProgressing {
		t.Fatalf("a retrying job is progressing at timeout, got %s (%s)", out, reason)
	}
}

// TestWedgedJobPodAtTimeoutIsFailed: a terminal waiting state is failure for
// every kind — retries cannot fix an image that does not pull.
func TestWedgedJobPodAtTimeoutIsFailed(t *testing.T) {
	tr := newTracker(opts())
	wedged := mkpod("wedge", "u1", false, 0)
	wedged.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	tr.observe(t0, completionSnap(status.InProgressStatus, "Job in progress", []*corev1.Pod{wedged}))
	out, reason := tr.timeoutVerdict()
	if out != OutcomeFailed || !strings.Contains(reason, "ImagePullBackOff") {
		t.Fatalf("wedged container must fail at timeout, got %s (%s)", out, reason)
	}
}
