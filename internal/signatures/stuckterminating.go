package signatures

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// stuckTerminatingSlack is how far past its grace period a deleting pod may
// linger before it counts as stuck; the slack absorbs kubelet and API churn.
const stuckTerminatingSlack = 30 * time.Second

// detectStuckTerminating fires on pods whose deletion has stopped making
// progress behind finalizers. It also runs on previous-revision pods: a
// rollout cannot finish while an old pod refuses to go away, and the
// finalizer holding it is the cause a human would otherwise dig for.
func detectStuckTerminating(c *Context, pod *corev1.Pod) *Finding {
	if pod.DeletionTimestamp == nil || len(pod.Finalizers) == 0 {
		return nil
	}
	var grace time.Duration
	if pod.DeletionGracePeriodSeconds != nil {
		grace = time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second
	}
	if c.Now().Before(pod.DeletionTimestamp.Add(grace + stuckTerminatingSlack)) {
		return nil
	}
	quoted := make([]string, len(pod.Finalizers))
	for i, fin := range pod.Finalizers {
		quoted[i] = strconv.Quote(fin)
	}
	noun := "finalizer"
	if len(quoted) > 1 {
		noun = "finalizers"
	}
	return &Finding{
		Signature: "PodStuckTerminating",
		Cause:     fmt.Sprintf("pod deletion is blocked by %s %s", noun, strings.Join(quoted, ", ")),
		Evidence: []Evidence{{
			Source: "status",
			Text: fmt.Sprintf("deletion requested at %s (grace period %ds); the pod is still present",
				pod.DeletionTimestamp.UTC().Format(time.RFC3339), int(grace/time.Second)),
		}},
	}
}
