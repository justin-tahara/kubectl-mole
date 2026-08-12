package signatures

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// detectContainerFailed fires on pods that ran and died for good: phase
// Failed with a non-zero container exit. Under restartPolicy Never — the
// shape of every Job retry — there is no crash loop and no restart count,
// just a dead pod holding its exit code and logs, so none of the
// restart-family detectors can claim it.
func detectContainerFailed(c *Context, pod *corev1.Pod) *Finding {
	if pod.Status.Phase != corev1.PodFailed {
		return nil
	}
	for _, cs := range allContainerStatuses(pod) {
		t := cs.State.Terminated
		if t == nil || t.ExitCode == 0 {
			continue
		}
		f := &Finding{
			Signature: "ContainerFailed",
			Cause:     fmt.Sprintf("%s %s exited with code %d", containerNoun(pod, cs.Name), cs.Name, t.ExitCode),
		}
		if t.Message != "" {
			f.Evidence = append(f.Evidence, Evidence{Source: "status", Text: clip(t.Message, maxEventEvidence)})
		}
		if logs := c.CrashLogs(pod, cs); logs != "" {
			f.Evidence = append(f.Evidence, Evidence{Source: "log", Text: clip(strings.TrimRight(logs, "\n"), maxLogEvidence)})
		}
		return f
	}
	return nil
}
