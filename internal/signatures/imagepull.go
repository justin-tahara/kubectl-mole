package signatures

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// detectImagePull fires on containers stuck pulling their image. The cause
// notes when registry authentication is implicated, since that is the fix in
// most real cases.
func detectImagePull(c *Context, pod *corev1.Pod) *Finding {
	for _, cs := range allContainerStatuses(pod) {
		w := cs.State.Waiting
		if w == nil || (w.Reason != "ImagePullBackOff" && w.Reason != "ErrImagePull") {
			continue
		}
		f := &Finding{
			Signature: "ImagePullBackOff",
			Cause:     fmt.Sprintf("%s %s cannot pull image %q", containerNoun(pod, cs.Name), cs.Name, imageFor(pod, cs.Name)),
		}
		auth := authIndicated(w.Message)
		if w.Message != "" {
			f.Evidence = append(f.Evidence, Evidence{Source: "status", Text: clip(w.Message, maxEventEvidence)})
		}
		if e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
			return e.Reason == "Failed" && strings.Contains(strings.ToLower(e.Message), "pull")
		}); e != nil {
			f.Evidence = append(f.Evidence, Evidence{Source: "event", Text: clip(e.Message, maxEventEvidence)})
			auth = auth || authIndicated(e.Message)
		}
		if auth {
			f.Cause += " (registry auth implicated)"
		}
		return f
	}
	return nil
}

func imageFor(pod *corev1.Pod, container string) string {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == container {
			return c.Image
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			return c.Image
		}
	}
	return "unknown"
}

// latestEventWhere returns the newest event matching pred; events arrive
// oldest-first from the orchestrator.
func latestEventWhere(events []corev1.Event, pred func(corev1.Event) bool) *corev1.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if pred(events[i]) {
			return &events[i]
		}
	}
	return nil
}
