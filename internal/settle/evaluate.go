package settle

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// terminalWaitingReasons are container waiting states that indicate a
// terminal failure rather than normal startup progress.
var terminalWaitingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"InvalidImageName":           true,
	"RunContainerError":          true,
}

// healthyNow reports whether the snapshot satisfies every settle condition at
// this instant. Holding that state for the stability window is the tracker's
// job.
func healthyNow(s snapshot) (bool, string) {
	if !s.found {
		return false, "workload not found"
	}
	// Guard: a controller that has not observed the current generation is
	// reporting status for the previous rollout. Never evaluate stale status.
	if s.observedGeneration < s.generation {
		return false, fmt.Sprintf("controller has not observed generation %d yet (observed %d)", s.generation, s.observedGeneration)
	}
	if s.kstatus.Status != status.CurrentStatus {
		return false, fmt.Sprintf("%s: %s", strings.ToLower(string(s.kstatus.Status)), s.kstatus.Message)
	}
	// Guard: controllers stop counting terminating pods long before they are
	// gone, so kstatus can report Current while previous-revision pods still
	// exist. Settled means the old state is actually gone.
	if len(s.oldPods) > 0 {
		return false, fmt.Sprintf("%d pod(s) from previous revisions still present", len(s.oldPods))
	}
	for _, p := range s.currentPods {
		if p.DeletionTimestamp != nil {
			return false, fmt.Sprintf("pod %s is terminating", p.Name)
		}
		if r := terminalPodReason(p); r != "" {
			return false, r
		}
		if !podReady(p) {
			return false, fmt.Sprintf("pod %s not ready", p.Name)
		}
	}
	return true, ""
}

// terminalPodReason describes a terminal-failure indicator on the pod, or
// returns "" if none is present.
func terminalPodReason(p *corev1.Pod) string {
	if p.Status.Phase == corev1.PodFailed {
		return fmt.Sprintf("pod %s in phase Failed", p.Name)
	}
	return terminalWaitingReason(p)
}

// terminalWaitingReason reports a container wedged in a terminal waiting
// state, the one indicator that is failure for every target kind — a Job
// retry can survive pods in phase Failed, but not an image that cannot pull.
func terminalWaitingReason(p *corev1.Pod) string {
	statuses := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	statuses = append(statuses, p.Status.InitContainerStatuses...)
	statuses = append(statuses, p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.State.Waiting != nil && terminalWaitingReasons[cs.State.Waiting.Reason] {
			return fmt.Sprintf("container %s of pod %s in %s", cs.Name, p.Name, cs.State.Waiting.Reason)
		}
	}
	return ""
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
