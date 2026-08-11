package signatures

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// detectPVCPending fires when a pod references a PersistentVolumeClaim that
// is stuck Pending. It runs before the unschedulable detector: the pending
// claim is the cause, the unschedulable pod its symptom.
func detectPVCPending(c *Context, pod *corev1.Pod) *Finding {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc := c.PVC(v.PersistentVolumeClaim.ClaimName)
		if pvc == nil || pvc.Status.Phase != corev1.ClaimPending {
			continue
		}
		sc := "none"
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
			sc = *pvc.Spec.StorageClassName
		}
		f := &Finding{
			Signature: "PVCPending",
			Cause:     fmt.Sprintf("PVC %q pending (storageClass %s): no PersistentVolume bound", pvc.Name, sc),
		}
		if e := latestEventWhere(c.PVCEvents(pvc.Name), func(corev1.Event) bool { return true }); e != nil {
			f.Evidence = append(f.Evidence, Evidence{Source: "event", Text: clip(e.Message, maxEventEvidence)})
		}
		return f
	}
	return nil
}
