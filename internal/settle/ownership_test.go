package settle

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func controlled(name, hash string, owner types.UID) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID("pod-" + name),
		Labels: map[string]string{},
	}}
	if hash != "" {
		p.Labels[controllerRevisionHashLabel] = hash
	}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{
			UID: owner, Controller: ptr.To(true), Kind: "x", APIVersion: "v1", Name: "o",
		}}
	}
	return p
}

// A sibling workload sharing the label selector must not have its pods
// counted as this workload's previous revisions — the paired
// linux/windows DaemonSet shape that held a healthy target at progressing
// for its whole timeout.
func TestSplitDropsSiblingPods(t *testing.T) {
	t.Run("daemonset", func(t *testing.T) {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "n", UID: "ds-1"}}
		cr := &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name: "agent-abc", Namespace: "n",
				Labels:          map[string]string{controllerRevisionHashLabel: "abc"},
				OwnerReferences: []metav1.OwnerReference{{UID: "ds-1", Controller: ptr.To(true), Kind: "DaemonSet", APIVersion: "apps/v1", Name: "agent"}},
			},
			Revision: 1,
		}
		factory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
		if err := factory.Apps().V1().ControllerRevisions().Informer().GetIndexer().Add(cr); err != nil {
			t.Fatal(err)
		}
		pods := []*corev1.Pod{
			controlled("mine-current", "abc", "ds-1"),
			controlled("mine-old", "zzz", "ds-1"),
			controlled("sibling", "other", "ds-2"), // the linux sibling's pod
			controlled("orphan", "", ""),           // adoptable: stays visible
		}
		current, old := splitDaemonSetPods(ds, factory.Apps().V1().ControllerRevisions().Lister(), pods)
		if len(current) != 1 || current[0].Name != "mine-current" {
			t.Fatalf("current = %v", names(current))
		}
		if len(old) != 2 || old[0].Name != "mine-old" || old[1].Name != "orphan" {
			t.Fatalf("old must keep our old pod and the orphan, drop the sibling: %v", names(old))
		}
	})

	t.Run("statefulset", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", UID: "sts-1"},
			Status:     appsv1.StatefulSetStatus{UpdateRevision: "abc"},
		}
		pods := []*corev1.Pod{
			controlled("mine-current", "abc", "sts-1"),
			controlled("sibling", "abc", "sts-2"),
		}
		current, old := splitStatefulSetPods(sts, pods)
		if len(current) != 1 || len(old) != 0 {
			t.Fatalf("sibling pod leaked: current=%v old=%v", names(current), names(old))
		}
	})

	t.Run("deployment", func(t *testing.T) {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "n", UID: "dep-1"}}
		rs := func(name string, uid, owner types.UID, rev string) *appsv1.ReplicaSet {
			return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "n", UID: uid,
				Annotations:     map[string]string{deploymentRevisionAnnotation: rev},
				OwnerReferences: []metav1.OwnerReference{{UID: owner, Controller: ptr.To(true), Kind: "Deployment", APIVersion: "apps/v1", Name: "o"}},
			}}
		}
		factory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
		idx := factory.Apps().V1().ReplicaSets().Informer().GetIndexer()
		for _, r := range []*appsv1.ReplicaSet{
			rs("api-new", "rs-new", "dep-1", "2"),
			rs("api-old", "rs-old", "dep-1", "1"),
			rs("sibling-rs", "rs-sib", "dep-2", "9"),
		} {
			if err := idx.Add(r); err != nil {
				t.Fatal(err)
			}
		}
		pods := []*corev1.Pod{
			controlled("mine-current", "", "rs-new"),
			controlled("mine-old", "", "rs-old"),
			controlled("sibling", "", "rs-sib"),
		}
		current, old := splitDeploymentPods(d, factory.Apps().V1().ReplicaSets().Lister(), pods)
		if len(current) != 1 || current[0].Name != "mine-current" {
			t.Fatalf("current = %v", names(current))
		}
		if len(old) != 1 || old[0].Name != "mine-old" {
			t.Fatalf("old must keep our old-revision pod, drop the sibling's: %v", names(old))
		}
	})
}

func names(pods []*corev1.Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}
	return out
}
