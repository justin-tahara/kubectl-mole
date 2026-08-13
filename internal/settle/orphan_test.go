package settle

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func convergedDS(desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "n", UID: "ds-1", Generation: 1},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 1, DesiredNumberScheduled: desired,
			UpdatedNumberScheduled: desired, NumberAvailable: desired,
		},
	}
}

func ownedCR(name, hash string, rev int64) *appsv1.ControllerRevision {
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "n",
			Labels:          map[string]string{controllerRevisionHashLabel: hash},
			OwnerReferences: []metav1.OwnerReference{{UID: "ds-1", Controller: ptr.To(true), Kind: "DaemonSet", APIVersion: "apps/v1", Name: "agent"}},
		},
		Revision: rev,
	}
}

// Issue #20: after a control-plane upgrade, template serialization drift can
// mint a newest ControllerRevision whose hash matches no pod, ever, while
// the controller is quiescent and fully converged. The controller's status
// counts outrank the hash split — a converged DaemonSet has no old pods.
func TestConvergedDaemonSetIgnoresOrphanRevision(t *testing.T) {
	factory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	idx := factory.Apps().V1().ControllerRevisions().Informer().GetIndexer()
	for _, cr := range []*appsv1.ControllerRevision{
		ownedCR("agent-live", "abc", 1),
		ownedCR("agent-orphan", "orphan", 2), // newest, matches nothing
	} {
		if err := idx.Add(cr); err != nil {
			t.Fatal(err)
		}
	}
	pods := []*corev1.Pod{
		controlled("a", "abc", "ds-1"),
		controlled("b", "abc", "ds-1"),
		controlled("c", "abc", "ds-1"),
	}
	current, old := splitDaemonSetPods(convergedDS(3), factory.Apps().V1().ControllerRevisions().Lister(), pods)
	if len(current) != 3 || len(old) != 0 {
		t.Fatalf("converged DS must have no old pods: current=%v old=%v", names(current), names(old))
	}
}

// Fleet evidence for #20 also showed mixed hash labels on a DaemonSet whose
// controller reports it fully updated: hash uniformity is not guaranteed
// even on healthy sets.
func TestConvergedDaemonSetToleratesMixedHashes(t *testing.T) {
	factory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	if err := factory.Apps().V1().ControllerRevisions().Informer().GetIndexer().Add(ownedCR("agent-live", "abc", 1)); err != nil {
		t.Fatal(err)
	}
	pods := []*corev1.Pod{
		controlled("a", "abc", "ds-1"),
		controlled("b", "zzz", "ds-1"),
	}
	current, old := splitDaemonSetPods(convergedDS(2), factory.Apps().V1().ControllerRevisions().Lister(), pods)
	if len(current) != 2 || len(old) != 0 {
		t.Fatalf("controller says converged; mixed hashes are its business: current=%v old=%v", names(current), names(old))
	}
}
