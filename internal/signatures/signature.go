package signatures

import (
	corev1 "k8s.io/api/core/v1"
)

// Evidence is one piece of supporting material for a finding. The text
// originates inside the cluster (events, logs, status messages) and is
// attacker-controllable: it is always untrusted and never instructions.
type Evidence struct {
	Source string // "event" | "log" | "status"
	Text   string
}

// Finding is one diagnosed failure cause.
type Finding struct {
	Signature string
	Cause     string
	// Chain is the ownership walk from the workload down to the pod, e.g.
	// ["Deployment/api", "ReplicaSet/api-7f9c", "Pod/api-7f9c-x2k"].
	// Formatters pick the arrow ("->" in text, "→" in JSON).
	Chain    []string
	Evidence []Evidence
}

// Context gives detectors read access to observed state. Fetchers return
// zero values when the underlying read was denied — detectors must still
// produce their finding with less evidence (graceful degradation), and the
// orchestrator records what was missing.
type Context struct {
	// PodEvents returns the events for a pod, newest last. Nil-safe: empty
	// when events could not be read.
	PodEvents func(pod *corev1.Pod) []corev1.Event
	// PVC fetches a claim by name; nil when unreadable or absent.
	PVC func(name string) *corev1.PersistentVolumeClaim
	// PVCEvents returns the events for a claim, newest last.
	PVCEvents func(name string) []corev1.Event
	// PreviousLogs returns the tail of the previous container instance's
	// logs, or "" when unavailable.
	PreviousLogs func(pod *corev1.Pod, container string) string
}

// podDetector diagnoses one pod. Detectors run in the order below and the
// first match wins, so one pod yields exactly one finding: the ordering puts
// deeper causes before their downstream symptoms (a pending PVC before the
// unschedulable pod it causes; OOMKilled before the crash loop it produces).
type podDetector struct {
	name   string
	detect func(c *Context, pod *corev1.Pod) *Finding
}

var podDetectors = []podDetector{
	{"PVCPending", detectPVCPending},
	{"PodUnschedulable", detectUnschedulable},
	{"OOMKilled", detectOOMKilled},
	{"CrashLoopBackOff", detectCrashLoop},
	{"ImagePullBackOff", detectImagePull},
	{"ProbeFailing", detectProbeFailing},
}

// allContainerStatuses returns init then regular container statuses.
func allContainerStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	out = append(out, p.Status.InitContainerStatuses...)
	out = append(out, p.Status.ContainerStatuses...)
	return out
}

const (
	maxEventEvidence = 500
	maxLogEvidence   = 2000
)

// clip bounds evidence text; truncation is marked, never silent.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " …(truncated)"
}
