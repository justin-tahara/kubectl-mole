package signatures

import (
	"strings"
	"time"

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
	// Pod names the pod a pod-level finding anchors to; empty for
	// workload-level findings (admission, quota).
	Pod string
	// Namespace of the diagnosed workload. Set by Diagnose; the collapse
	// layer needs it to keep anchors distinct across a fan-out.
	Namespace string
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
	// CrashLogs returns the log tail of the container's most recent crashed
	// instance, or "" when unavailable.
	CrashLogs func(pod *corev1.Pod, status corev1.ContainerStatus) string
	// Node fetches a node by name; nil when unreadable or absent.
	Node func(name string) *corev1.Node
	// Now is the wall clock, injectable for tests. Only detectors that
	// judge whether a state has overstayed (stuck terminating) consult it.
	Now func() time.Time
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
	{"NodeNotReady", detectNodeNotReady},
	{"PodStuckTerminating", detectStuckTerminating},
	{"PVCPending", detectPVCPending},
	{"PodSandboxFailed", detectSandboxFailed},
	{"VolumeMountFailed", detectVolumeMountFailed},
	{"PodUnschedulable", detectUnschedulable},
	{"PodEvicted", detectEvicted},
	{"OOMKilled", detectOOMKilled},
	{"ConfigMissing", detectConfigMissing},
	{"ContainerStartFailed", detectStartFailed},
	{"CrashLoopBackOff", detectCrashLoop},
	{"ContainerFailed", detectContainerFailed},
	{"ImagePullBackOff", detectImagePull},
	{"ProbeFailing", detectProbeFailing},
}

// oldPodDetectors run on previous-revision pods. Old pods get exactly one
// question — are they wedging the rollout? — because their other symptoms
// are history: the pods are on the way out. A dead node underneath still
// outranks the finalizer, and folds with current-pod findings on that node.
var oldPodDetectors = []podDetector{
	{"NodeNotReady", detectNodeNotReady},
	{"PodStuckTerminating", detectStuckTerminating},
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

// containerNoun says which kind of container a cause names. Init-container
// failures read differently — the pod never leaves initialization — so a
// cause must not present one as an ordinary container.
func containerNoun(pod *corev1.Pod, name string) string {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return "init container"
		}
	}
	return "container"
}

// firstLine bounds multi-line cluster text to its first line for use in a
// cause; the full text belongs in evidence.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
