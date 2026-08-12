package signatures

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// detectConfigMissing fires when container config generation fails: a
// ConfigMap or Secret referenced by env or envFrom does not exist, or lacks
// the referenced key. The kubelet's waiting message already names the
// missing object, so it is the cause. (The same mistake behind a volume
// surfaces as VolumeMountFailed instead — the message names the object
// either way.)
func detectConfigMissing(_ *Context, pod *corev1.Pod) *Finding {
	for _, cs := range allContainerStatuses(pod) {
		w := cs.State.Waiting
		if w == nil || w.Reason != "CreateContainerConfigError" {
			continue
		}
		cause := fmt.Sprintf("%s %s cannot start (CreateContainerConfigError)", containerNoun(pod, cs.Name), cs.Name)
		if w.Message != "" {
			cause = fmt.Sprintf("%s %s cannot start: %s", containerNoun(pod, cs.Name), cs.Name, clip(w.Message, maxEventEvidence))
		}
		return &Finding{Signature: "ConfigMissing", Cause: cause}
	}
	return nil
}
