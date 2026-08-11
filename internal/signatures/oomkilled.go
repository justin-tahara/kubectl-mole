package signatures

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// detectOOMKilled fires when a container was killed by the OOM killer. It
// runs before the crash-loop detector: OOMKilled with restartPolicy Always
// presents as CrashLoopBackOff, and the memory limit is the actionable fact.
func detectOOMKilled(_ *Context, pod *corev1.Pod) *Finding {
	for _, cs := range allContainerStatuses(pod) {
		t := cs.State.Terminated
		if t == nil || t.Reason != "OOMKilled" {
			t = cs.LastTerminationState.Terminated
		}
		if t == nil || t.Reason != "OOMKilled" {
			continue
		}
		return &Finding{
			Signature: "OOMKilled",
			Cause:     fmt.Sprintf("container %s OOMKilled (memory limit %s)", cs.Name, memoryLimit(pod, cs.Name)),
			Evidence: []Evidence{{
				Source: "status",
				Text:   fmt.Sprintf("last termination: reason %s, exit code %d, %d restart(s)", t.Reason, t.ExitCode, cs.RestartCount),
			}},
		}
	}
	return nil
}

func memoryLimit(pod *corev1.Pod, container string) string {
	all := make([]corev1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	all = append(all, pod.Spec.InitContainers...)
	all = append(all, pod.Spec.Containers...)
	for _, c := range all {
		if c.Name != container {
			continue
		}
		if l, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return l.String()
		}
	}
	return "not set"
}
