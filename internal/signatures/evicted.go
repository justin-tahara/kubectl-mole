package signatures

import (
	corev1 "k8s.io/api/core/v1"
)

// detectEvicted fires on pods the kubelet evicted — node pressure, or the
// pod exceeding its own ephemeral-storage limit. The eviction verdict lands
// in status.reason and status.message on the failed pod, which lingers as
// the record of what happened while its replacement (often) repeats it.
func detectEvicted(c *Context, pod *corev1.Pod) *Finding {
	if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "Evicted" {
		return nil
	}
	f := &Finding{Signature: "PodEvicted", Cause: "pod was evicted"}
	if pod.Status.Message != "" {
		f.Cause = "pod was evicted: " + clip(firstLine(pod.Status.Message), maxEventEvidence)
		f.Evidence = append(f.Evidence, Evidence{Source: "status", Text: clip(pod.Status.Message, maxEventEvidence)})
	}
	if e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
		return e.Reason == "Evicted"
	}); e != nil {
		f.Evidence = append(f.Evidence, Evidence{Source: "event", Text: clip(e.Message, maxEventEvidence)})
	}
	return f
}
