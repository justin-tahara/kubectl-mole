package settle

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// tracker owns the stability window. It consumes timestamped snapshots and
// decides when the watch is finished; it does no I/O and holds no clocks, so
// tests drive it with fabricated times.
type tracker struct {
	opts Options

	// healthySince marks when the current uninterrupted healthy streak
	// began; zero when unhealthy.
	healthySince time.Time
	// baseline holds per-pod restart counts captured when the current
	// stability window opened. Any increase, or any change in pod set
	// membership, restarts the window.
	baseline map[types.UID]int32

	// seen tracks the highest restart count observed per pod across the
	// whole watch, to classify a never-settling target at timeout.
	seen            map[types.UID]int32
	restartObserved string

	// wedged accumulates, per current pod, time spent in a state that
	// timeoutVerdict would already call failed. Ready wipes a pod's entry —
	// real recovery earns fresh patience — while the ambiguous gaps of a
	// crash-backoff cycle (briefly Running, not Ready) neither add nor
	// reset. When an entry reaches opts.WedgedFor, the watch ends failed
	// early: the deadline verdict was already decided, waiting only delays
	// it.
	wedged      map[types.UID]time.Duration
	lastObserve time.Time
	// wedgedOut marks that the watch ended through the wedged-for window
	// rather than a controller condition or the deadline. Surfaced as the
	// verdict's structured earlyExit/wedgedFor fields — never woven into
	// the reason string, which stays the cause alone.
	wedgedOut bool

	lastReason string
	lastSnap   snapshot
}

func newTracker(opts Options) *tracker {
	return &tracker{opts: opts, seen: map[types.UID]int32{}}
}

// observe consumes one snapshot. done is true when the verdict is final
// before timeout: settled, or a terminal kstatus failure.
func (t *tracker) observe(now time.Time, s snapshot) (Outcome, bool) {
	t.lastSnap = s
	t.noteRestarts(s.currentPods)

	if s.terminalFailure != "" {
		t.lastReason = "failed: " + s.terminalFailure
		return OutcomeFailed, true
	}
	if s.kstatus.Status == status.FailedStatus {
		t.lastReason = fmt.Sprintf("failed: %s", s.kstatus.Message)
		return OutcomeFailed, true
	}
	if r, wedged := t.observeWedge(now, s); wedged {
		t.lastReason = "failed: " + r
		return OutcomeFailed, true
	}
	// Completion-terminal targets bypass the readiness path entirely:
	// completion cannot regress, so Current settles with no stability
	// window, and pod-level churn (retry pods, restarts) is progress.
	if s.completionTerminal {
		if s.kstatus.Status == status.CurrentStatus {
			t.lastReason = "completed"
			if s.kstatus.Message != "" {
				t.lastReason = "completed: " + s.kstatus.Message
			}
			return OutcomeSettled, true
		}
		t.lastReason = s.kstatus.Message
		if t.lastReason == "" {
			t.lastReason = "not complete yet"
		}
		return "", false
	}

	healthy, reason := healthyNow(s)
	if !healthy {
		t.healthySince = time.Time{}
		t.baseline = nil
		t.lastReason = reason
		return "", false
	}

	counts := restartCounts(s.currentPods)
	if t.healthySince.IsZero() {
		t.healthySince = now
		t.baseline = counts
		t.lastReason = fmt.Sprintf("healthy, holding for stability window (%s)", t.opts.StableFor)
		return "", false
	}
	if v := windowViolation(t.baseline, s.currentPods, counts); v != "" {
		t.healthySince = now
		t.baseline = counts
		t.lastReason = v + " during stability window; window restarted"
		return "", false
	}
	if now.Sub(t.healthySince) >= t.opts.StableFor {
		t.lastReason = fmt.Sprintf("healthy for %s", t.opts.StableFor)
		if s.note != "" {
			t.lastReason += " (" + s.note + ")"
		}
		return OutcomeSettled, true
	}
	return "", false
}

func (t *tracker) observation() Observation {
	return Observation{CurrentPods: t.lastSnap.currentPods, OldPods: t.lastSnap.oldPods}
}

// observeWedge advances the per-pod wedged clocks and reports whether one
// crossed the early-failure window. The wedge signal mirrors timeoutVerdict
// exactly — a terminal waiting reason for every kind, phase Failed only for
// targets that settle by readiness (Job retries fail pods routinely) — so
// anything failed here would have been failed at the deadline anyway.
func (t *tracker) observeWedge(now time.Time, s snapshot) (string, bool) {
	var dt time.Duration
	if !t.lastObserve.IsZero() {
		dt = now.Sub(t.lastObserve)
	}
	t.lastObserve = now
	if t.opts.WedgedFor <= 0 {
		return "", false
	}
	for _, p := range s.currentPods {
		var reason string
		if s.completionTerminal {
			reason = terminalWaitingReason(p)
		} else {
			reason = terminalPodReason(p)
		}
		if reason == "" {
			if podReady(p) {
				delete(t.wedged, p.UID)
			}
			continue
		}
		if t.wedged == nil {
			t.wedged = map[types.UID]time.Duration{}
		}
		t.wedged[p.UID] += dt
		if t.wedged[p.UID] >= t.opts.WedgedFor {
			t.wedgedOut = true
			return reason, true
		}
	}
	return "", false
}

// timeoutVerdict classifies a watch that hit its timeout. Failed requires a
// concrete terminal indicator; everything else is progressing — conflating
// the two is how automation rolls back a deployment that was 30 seconds from
// healthy. For completion-terminal targets only a wedged container counts:
// pods in phase Failed and restarts are how retries look, and the controller
// has not declared the Job failed.
func (t *tracker) timeoutVerdict() (Outcome, string) {
	for _, p := range t.lastSnap.currentPods {
		if r := terminalWaitingReason(p); r != "" {
			return OutcomeFailed, r
		}
		if !t.lastSnap.completionTerminal && p.Status.Phase == corev1.PodFailed {
			return OutcomeFailed, fmt.Sprintf("pod %s in phase Failed", p.Name)
		}
	}
	if !t.lastSnap.completionTerminal && t.restartObserved != "" {
		return OutcomeFailed, t.restartObserved + " and the workload did not settle"
	}
	reason := t.lastReason
	if reason == "" {
		reason = "did not settle before timeout"
	}
	return OutcomeProgressing, "still progressing: " + reason
}

// noteRestarts records restart-count increases seen at any point in the
// watch, including pods that appear mid-watch already restarted.
func (t *tracker) noteRestarts(pods []*corev1.Pod) {
	for _, p := range pods {
		n := podRestarts(p)
		prev, known := t.seen[p.UID]
		if (known && n > prev) || (!known && n > 0) {
			t.restartObserved = fmt.Sprintf("pod %s restarted during watch", p.Name)
		}
		if !known || n > prev {
			t.seen[p.UID] = n
		}
	}
}

// windowViolation reports the first change that invalidates the open window.
// Iterates the sorted pod slice, never a map, for deterministic messages.
func windowViolation(baseline map[types.UID]int32, pods []*corev1.Pod, counts map[types.UID]int32) string {
	if len(counts) != len(baseline) {
		return "pod set changed"
	}
	for _, p := range pods {
		b, ok := baseline[p.UID]
		if !ok {
			return fmt.Sprintf("pod %s appeared", p.Name)
		}
		if counts[p.UID] > b {
			return fmt.Sprintf("pod %s restarted", p.Name)
		}
	}
	return ""
}

func restartCounts(pods []*corev1.Pod) map[types.UID]int32 {
	c := make(map[types.UID]int32, len(pods))
	for _, p := range pods {
		c[p.UID] = podRestarts(p)
	}
	return c
}

func podRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, cs := range p.Status.InitContainerStatuses {
		n += cs.RestartCount
	}
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}
