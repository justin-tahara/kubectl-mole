package signatures

import (
	corev1 "k8s.io/api/core/v1"
)

// detectUnschedulable fires on pods the scheduler cannot place. The
// condition message already names the specific unsatisfied predicates
// ("0/5 nodes are available: 5 Insufficient cpu"), so it is the cause.
func detectUnschedulable(c *Context, pod *corev1.Pod) *Finding {
	for _, cond := range pod.Status.Conditions {
		if cond.Type != corev1.PodScheduled || cond.Status != corev1.ConditionFalse || cond.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		f := &Finding{
			Signature: "PodUnschedulable",
			Cause:     "cannot schedule: " + clip(cond.Message, maxEventEvidence),
		}
		if e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
			return e.Reason == "FailedScheduling"
		}); e != nil {
			f.Evidence = append(f.Evidence, Evidence{Source: "event", Text: clip(e.Message, maxEventEvidence)})
		}
		return f
	}
	return nil
}
