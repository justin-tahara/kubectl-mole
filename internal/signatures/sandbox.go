package signatures

import (
	corev1 "k8s.io/api/core/v1"
)

// detectSandboxFailed fires when the pod sandbox cannot be created: CNI or
// container-runtime failures on the node (no IPs left to assign, a broken
// network plugin). The only trace is the kubelet's FailedCreatePodSandBox
// event — nothing about it appears in container statuses.
func detectSandboxFailed(c *Context, pod *corev1.Pod) *Finding {
	if podIsReady(pod) || !waitingToStart(pod) {
		return nil
	}
	e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
		return e.Reason == "FailedCreatePodSandBox"
	})
	if e == nil {
		return nil
	}
	return &Finding{
		Signature: "PodSandboxFailed",
		Cause:     "cannot create the pod sandbox: " + clip(e.Message, maxEventEvidence),
	}
}
