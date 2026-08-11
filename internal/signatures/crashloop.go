package signatures

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// detectCrashLoop fires on containers in CrashLoopBackOff, attaching the
// previous container instance's log tail and exit code — the two things a
// human would go dig for next.
func detectCrashLoop(c *Context, pod *corev1.Pod) *Finding {
	for _, cs := range allContainerStatuses(pod) {
		w := cs.State.Waiting
		if w == nil || w.Reason != "CrashLoopBackOff" {
			continue
		}
		f := &Finding{Signature: "CrashLoopBackOff"}
		f.Cause = fmt.Sprintf("container %s is crash-looping", cs.Name)
		if t := cs.LastTerminationState.Terminated; t != nil {
			f.Cause = fmt.Sprintf("container %s is crash-looping (last exit code %d)", cs.Name, t.ExitCode)
		}
		if e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
			return e.Reason == "Unhealthy" && strings.HasPrefix(e.Message, "Liveness")
		}); e != nil {
			f.Cause += "; restarts driven by failing liveness probe"
			f.Evidence = append(f.Evidence, Evidence{Source: "event", Text: clip(e.Message, maxEventEvidence)})
		}
		if logs := c.PreviousLogs(pod, cs.Name); logs != "" {
			f.Evidence = append(f.Evidence, Evidence{Source: "log", Text: clip(strings.TrimRight(logs, "\n"), maxLogEvidence)})
		}
		return f
	}
	return nil
}
