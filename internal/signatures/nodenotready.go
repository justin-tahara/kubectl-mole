package signatures

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// detectNodeNotReady attributes an unready pod to its node when the node
// itself is not ready. It runs before every other detector: whatever symptom
// the pod exhibits, a dead node underneath it is the deeper cause. The cause
// string names the node and never the pod, so every pod on that node
// produces an identical finding and the collapse layer folds them into one
// entry.
func detectNodeNotReady(c *Context, pod *corev1.Pod) *Finding {
	if pod.Spec.NodeName == "" || podIsReady(pod) {
		return nil
	}
	n := c.Node(pod.Spec.NodeName)
	if n == nil {
		return nil
	}
	cond := nodeReadyCondition(n)
	if cond == nil || cond.Status == corev1.ConditionTrue {
		return nil
	}
	reason := cond.Reason
	if reason == "" {
		reason = "Ready condition " + string(cond.Status)
	}
	f := &Finding{
		Signature: "NodeNotReady",
		Cause:     fmt.Sprintf("node %q is not ready (%s)", n.Name, reason),
	}
	if cond.Message != "" {
		f.Evidence = append(f.Evidence, Evidence{Source: "status", Text: clip(cond.Message, maxEventEvidence)})
	}
	return f
}

func nodeReadyCondition(n *corev1.Node) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}
