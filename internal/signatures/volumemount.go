package signatures

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// detectVolumeMountFailed fires on pods stuck attaching or mounting their
// volumes. The kubelet and the attach/detach controller write the concrete
// failure — a volume still attached to another node, a missing Secret or
// ConfigMap behind a volume, a CSI timeout — into FailedMount and
// FailedAttachVolume events. Gated on the pod still waiting to start, so a
// stale mount event from startup cannot outrank the pod's real current
// failure.
func detectVolumeMountFailed(c *Context, pod *corev1.Pod) *Finding {
	if podIsReady(pod) || !waitingToStart(pod) {
		return nil
	}
	e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
		return e.Reason == "FailedMount" || e.Reason == "FailedAttachVolume"
	})
	if e == nil {
		return nil
	}
	verb := "mount"
	if e.Reason == "FailedAttachVolume" {
		verb = "attach"
	}
	return &Finding{
		Signature: "VolumeMountFailed",
		Cause:     fmt.Sprintf("cannot %s volumes: %s", verb, clip(e.Message, maxEventEvidence)),
	}
}

// waitingToStart reports whether a scheduled pod has not started any
// container yet: no statuses at all, or every container still waiting in a
// creation state. Mount, attach, and sandbox failures all present this way,
// and never apply to a pod whose containers have already run.
func waitingToStart(pod *corev1.Pod) bool {
	if pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, cs := range allContainerStatuses(pod) {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return false
		}
		if w := cs.State.Waiting; w != nil && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			return false
		}
	}
	return true
}
