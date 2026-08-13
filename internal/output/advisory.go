package output

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Advisory is one informational note on an otherwise-clean verdict. Kind
// names what it reports — a stable enum, today only "recent-restarts" —
// and the typed fields carry the machine-readable evidence: automation
// ranks and filters on them, Text is the display sentence formatters
// render, and neither channel ever parses the other. (The same rule that
// keeps CLI concerns out of machine-readable fields, applied in reverse:
// display formatting is not a machine interface.)
type Advisory struct {
	Kind string `json:"kind"`
	// Context is the kubeconfig context of a --contexts run; Target and
	// Namespace name the workload on fan-out verdicts. All omitted on a
	// single-target verdict, whose header already says.
	Context   string `json:"context,omitempty"`
	Target    string `json:"target,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// recent-restarts evidence. LastExitCode is a pointer because exit 0
	// is a real exit code — absent means the field does not apply to this
	// advisory kind, never that the code was zero. LastReason is the
	// kubelet's termination reason for the same instance (OOMKilled,
	// Error, ...) — the field that tells a memory kill from a liveness
	// kill when both exit 137; absent when the kubelet recorded none.
	TerminationsInWindow int    `json:"terminationsInWindow,omitempty"`
	Window               string `json:"window,omitempty"`
	LastExitCode         *int32 `json:"lastExitCode,omitempty"`
	LastReason           string `json:"lastReason,omitempty"`
	LastTerminatedAgo    string `json:"lastTerminatedAgo,omitempty"`
	LifetimeRestarts     int32  `json:"lifetimeRestarts,omitempty"`
	// ObservableHistory qualifies the window: present only when every pod
	// is younger than the window, carrying the oldest pod's age. On a
	// continuously-deployed cluster every rollout replaces pods and resets
	// restart counts, so "1 termination in 24h" read from 40-minute-old
	// pods is a 40-minute claim, not a 24-hour one.
	ObservableHistory string `json:"observableHistory,omitempty"`
	// Evidence is the previous-instance log tail behind the freshest
	// termination, when it is still fetchable — the one artifact that
	// explains a recovered crash, and the first thing kubelet GC or the
	// next rollout erases. Same untrusted marking as failure evidence.
	Evidence []Evidence `json:"evidence,omitempty"`
	Text     string     `json:"text"`
}

// RecentRestarts summarizes fresh termination evidence across pods: how
// many containers' last termination finished inside the window, the
// freshest of them (reason, exit code, age), and the lifetime restart
// total.
// Returns nil when nothing terminated inside the window — ancient restarts
// stay quiet. The gate is recency, not count: dogfood evidence showed a
// six-restart pod whose last crash was 27 days old (must not annotate)
// against four same-day liveness kills (must), and lifetime counts alone
// cannot tell them apart.
func RecentRestarts(pods []*corev1.Pod, window time.Duration, now time.Time) *Advisory {
	if window <= 0 {
		return nil
	}
	var (
		recent   int
		lifetime int32
		freshest time.Time
		exitCode int32
		reason   string
		oldest   time.Time
	)
	for _, p := range pods {
		if c := p.CreationTimestamp.Time; !c.IsZero() && (oldest.IsZero() || c.Before(oldest)) {
			oldest = c
		}
		for _, cs := range allStatuses(p) {
			lifetime += cs.RestartCount
			t := cs.LastTerminationState.Terminated
			if t == nil || t.FinishedAt.IsZero() {
				continue
			}
			if now.Sub(t.FinishedAt.Time) <= window {
				recent++
				if t.FinishedAt.After(freshest) {
					freshest = t.FinishedAt.Time
					exitCode = t.ExitCode
					reason = t.Reason
				}
			}
		}
	}
	if recent == 0 {
		return nil
	}
	last := fmt.Sprintf("exit %d, %s ago", exitCode, coarseAge(now.Sub(freshest)))
	if reason != "" {
		last = reason + ", " + last
	}
	text := fmt.Sprintf("containers restarted recently — %d termination(s) in last %s (last: %s); lifetime restarts across pods: %d",
		recent, coarseAge(window), last, lifetime)
	// Horizon honesty: the window claim is only as old as the oldest pod.
	var horizon string
	if age := now.Sub(oldest); !oldest.IsZero() && age < window {
		horizon = coarseAge(age)
		text += fmt.Sprintf("; note: pods younger than window (%s) — earlier history not visible", horizon)
	}
	code := exitCode
	return &Advisory{
		Kind:                 "recent-restarts",
		TerminationsInWindow: recent,
		Window:               coarseAge(window),
		LastExitCode:         &code,
		LastReason:           reason,
		LastTerminatedAgo:    coarseAge(now.Sub(freshest)),
		LifetimeRestarts:     lifetime,
		ObservableHistory:    horizon,
		Text:                 text,
	}
}

// FreshestTermination selects the crash a recent-restarts advisory speaks
// about: across the pods, the container whose last termination finished
// most recently inside the window. It is the selection rule RecentRestarts
// applies, exported so the evidence fetch reads the same crash the
// advisory names. Nil when nothing terminated inside the window.
func FreshestTermination(pods []*corev1.Pod, window time.Duration, now time.Time) (*corev1.Pod, *corev1.ContainerStatus) {
	var (
		pod      *corev1.Pod
		status   *corev1.ContainerStatus
		freshest time.Time
	)
	for _, p := range pods {
		statuses := allStatuses(p)
		for i := range statuses {
			t := statuses[i].LastTerminationState.Terminated
			if t == nil || t.FinishedAt.IsZero() || now.Sub(t.FinishedAt.Time) > window {
				continue
			}
			if t.FinishedAt.After(freshest) {
				freshest = t.FinishedAt.Time
				pod, status = p, &statuses[i]
			}
		}
	}
	return pod, status
}

func allStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	s := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	s = append(s, p.Status.InitContainerStatuses...)
	return append(s, p.Status.ContainerStatuses...)
}

// coarseAge renders a duration at advisory precision: whole minutes under
// an hour, whole hours after. The advisory is a pointer, not a measurement.
func coarseAge(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
