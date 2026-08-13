package output

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// RecentRestarts summarizes fresh termination evidence across pods: how
// many containers' last termination finished inside the window, the
// freshest of them (exit code and age), and the lifetime restart total.
// Returns "" when nothing terminated inside the window — ancient restarts
// stay quiet. The gate is recency, not count: dogfood evidence showed a
// six-restart pod whose last crash was 27 days old (must not annotate)
// against four same-day liveness kills (must), and lifetime counts alone
// cannot tell them apart.
func RecentRestarts(pods []*corev1.Pod, window time.Duration, now time.Time) string {
	if window <= 0 {
		return ""
	}
	var (
		recent   int
		lifetime int32
		freshest time.Time
		exitCode int32
	)
	for _, p := range pods {
		statuses := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
		statuses = append(statuses, p.Status.InitContainerStatuses...)
		statuses = append(statuses, p.Status.ContainerStatuses...)
		for _, cs := range statuses {
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
				}
			}
		}
	}
	if recent == 0 {
		return ""
	}
	return fmt.Sprintf("containers restarted recently — %d termination(s) in last %s (last: exit %d, %s ago); lifetime restarts across pods: %d",
		recent, coarseAge(window), exitCode, coarseAge(now.Sub(freshest)), lifetime)
}

// coarseAge renders a duration at advisory precision: whole minutes under
// an hour, whole hours after. The advisory is a pointer, not a measurement.
func coarseAge(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
