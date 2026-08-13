package settle

import (
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	appslisters "k8s.io/client-go/listers/apps/v1"
)

// foreignPod reports a pod whose controlling owner is some other workload —
// the sibling-DaemonSet shape, where two workloads share one label selector
// (a paired linux/windows DaemonSet is the wild example). Such a pod is not
// ours to judge: counting it as a previous-revision pod holds a healthy
// target at progressing forever. A pod with no controller stays visible —
// controllers adopt matching orphans, so one can genuinely wedge a rollout.
func foreignPod(p *corev1.Pod, owners ...types.UID) bool {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return false
	}
	for _, uid := range owners {
		if ref.UID == uid {
			return false
		}
	}
	return true
}

const (
	deploymentRevisionAnnotation = "deployment.kubernetes.io/revision"
	controllerRevisionHashLabel  = "controller-revision-hash"
)

// splitDeploymentPods separates pods of the deployment's newest ReplicaSet
// from pods of older ReplicaSets (or with no known owner). Any existing
// non-current pod — including one that is merely terminating — counts as
// old; the old pods come back as objects because diagnosis needs to see a
// pod that is wedging the rollout, not just count it.
func splitDeploymentPods(d *appsv1.Deployment, rsLister appslisters.ReplicaSetLister, pods []*corev1.Pod) ([]*corev1.Pod, []*corev1.Pod) {
	rss, err := rsLister.ReplicaSets(d.Namespace).List(labels.Everything())
	if err != nil {
		return nil, pods
	}
	var owned []*appsv1.ReplicaSet
	for _, rs := range rss {
		if ref := metav1.GetControllerOf(rs); ref != nil && ref.UID == d.UID {
			owned = append(owned, rs)
		}
	}
	if len(owned) == 0 {
		return nil, pods
	}
	sort.Slice(owned, func(i, j int) bool {
		ri, rj := rsRevision(owned[i]), rsRevision(owned[j])
		if ri != rj {
			return ri < rj
		}
		return owned[i].CreationTimestamp.Before(&owned[j].CreationTimestamp)
	})
	current := owned[len(owned)-1]

	ownedUIDs := make([]types.UID, 0, len(owned))
	for _, rs := range owned {
		ownedUIDs = append(ownedUIDs, rs.UID)
	}

	var currentPods, old []*corev1.Pod
	for _, p := range pods {
		if foreignPod(p, ownedUIDs...) {
			continue
		}
		if ref := metav1.GetControllerOf(p); ref != nil && ref.UID == current.UID {
			currentPods = append(currentPods, p)
		} else {
			old = append(old, p)
		}
	}
	return currentPods, old
}

func rsRevision(rs *appsv1.ReplicaSet) int64 {
	n, err := strconv.ParseInt(rs.Annotations[deploymentRevisionAnnotation], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// splitStatefulSetPods separates pods carrying the StatefulSet's update
// revision from everything else.
func splitStatefulSetPods(sts *appsv1.StatefulSet, pods []*corev1.Pod) ([]*corev1.Pod, []*corev1.Pod) {
	rev := sts.Status.UpdateRevision
	if rev == "" {
		return nil, pods
	}
	var current, old []*corev1.Pod
	for _, p := range pods {
		if foreignPod(p, sts.UID) {
			continue
		}
		if p.Labels[controllerRevisionHashLabel] == rev {
			current = append(current, p)
		} else {
			old = append(old, p)
		}
	}
	return current, old
}

// dsConverged is the DaemonSet controller's own statement that every pod
// is scheduled, updated, and available for the observed generation. It
// outranks the hash split: a control-plane upgrade can orphan the newest
// ControllerRevision (template serialization drift mints a revision that
// matches no pods, ever), and hash-label uniformity is not guaranteed even
// on healthy sets. Max-revision-hash is mole's derivation of "current";
// the status counts are the authority's.
func dsConverged(ds *appsv1.DaemonSet) bool {
	return ds.Status.ObservedGeneration >= ds.Generation &&
		ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled
}

// splitDaemonSetPods separates pods carrying the DaemonSet's newest
// ControllerRevision hash from everything else. When the controller
// reports full convergence, every owned pod is current and the hash is
// not consulted; the split only classifies during a rollout the
// controller itself says is in flight.
func splitDaemonSetPods(ds *appsv1.DaemonSet, crLister appslisters.ControllerRevisionLister, pods []*corev1.Pod) ([]*corev1.Pod, []*corev1.Pod) {
	if dsConverged(ds) {
		var current []*corev1.Pod
		for _, p := range pods {
			if foreignPod(p, ds.UID) {
				continue
			}
			current = append(current, p)
		}
		return current, nil
	}
	crs, err := crLister.ControllerRevisions(ds.Namespace).List(labels.Everything())
	if err != nil {
		return nil, pods
	}
	var newest *appsv1.ControllerRevision
	for _, cr := range crs {
		ref := metav1.GetControllerOf(cr)
		if ref == nil || ref.UID != ds.UID {
			continue
		}
		if newest == nil || cr.Revision > newest.Revision {
			newest = cr
		}
	}
	if newest == nil {
		return nil, pods
	}
	hash := newest.Labels[controllerRevisionHashLabel]

	var current, old []*corev1.Pod
	for _, p := range pods {
		if foreignPod(p, ds.UID) {
			continue
		}
		if hash != "" && p.Labels[controllerRevisionHashLabel] == hash {
			current = append(current, p)
		} else {
			old = append(old, p)
		}
	}
	return current, old
}
