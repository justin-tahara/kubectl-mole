package settle

import (
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appslisters "k8s.io/client-go/listers/apps/v1"
)

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

	var currentPods, old []*corev1.Pod
	for _, p := range pods {
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
		if p.Labels[controllerRevisionHashLabel] == rev {
			current = append(current, p)
		} else {
			old = append(old, p)
		}
	}
	return current, old
}

// splitDaemonSetPods separates pods carrying the DaemonSet's newest
// ControllerRevision hash from everything else.
func splitDaemonSetPods(ds *appsv1.DaemonSet, crLister appslisters.ControllerRevisionLister, pods []*corev1.Pod) ([]*corev1.Pod, []*corev1.Pod) {
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
		if hash != "" && p.Labels[controllerRevisionHashLabel] == hash {
			current = append(current, p)
		} else {
			old = append(old, p)
		}
	}
	return current, old
}
