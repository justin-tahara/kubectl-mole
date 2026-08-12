package signatures

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// startFailureReasons are the states runtimes report when a container cannot
// start at all — the process never ran, so there are no logs to fetch.
// containerd reports StartError; older runtimes used ContainerCannotRun and
// RunContainerError; CreateContainerError is the kubelet's own create
// failure.
var startFailureReasons = map[string]bool{
	"StartError":           true,
	"ContainerCannotRun":   true,
	"CreateContainerError": true,
	"RunContainerError":    true,
}

// detectStartFailed fires when a container cannot start at all — the classic
// case is an image that lacks the configured executable. It runs before the
// crash-loop detector: repeated start failures present as CrashLoopBackOff,
// and "the binary does not exist" is the actionable fact, not the loop.
func detectStartFailed(_ *Context, pod *corev1.Pod) *Finding {
	for _, cs := range allContainerStatuses(pod) {
		var reason, msg string
		switch {
		case cs.State.Waiting != nil && startFailureReasons[cs.State.Waiting.Reason]:
			reason, msg = cs.State.Waiting.Reason, cs.State.Waiting.Message
		case cs.State.Terminated != nil && startFailureReasons[cs.State.Terminated.Reason]:
			reason, msg = cs.State.Terminated.Reason, cs.State.Terminated.Message
		// The last termination is only current history while the pod is
		// still unready; a recovered pod keeps the old record around.
		case cs.LastTerminationState.Terminated != nil && startFailureReasons[cs.LastTerminationState.Terminated.Reason] && !podIsReady(pod):
			reason, msg = cs.LastTerminationState.Terminated.Reason, cs.LastTerminationState.Terminated.Message
		default:
			continue
		}
		f := &Finding{
			Signature: "ContainerStartFailed",
			Cause:     fmt.Sprintf("%s %s cannot start (%s)", containerNoun(pod, cs.Name), cs.Name, reason),
		}
		if msg != "" {
			f.Cause = fmt.Sprintf("%s %s cannot start: %s", containerNoun(pod, cs.Name), cs.Name, clip(firstLine(msg), maxEventEvidence))
			f.Evidence = append(f.Evidence, Evidence{Source: "status", Text: clip(msg, maxEventEvidence)})
		}
		return f
	}
	return nil
}
