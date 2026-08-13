//go:build e2e

package settle_test

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

// Issue #20: a control-plane upgrade can leave a DaemonSet with a newest
// ControllerRevision whose hash matches no pod, ever, while the controller
// is quiescent and converged. Planting exactly that orphan on a healthy
// DaemonSet must not stop it settling.
func TestConvergedDaemonSetWithOrphanRevisionSettles(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)

	labels := map[string]string{"app": "agent"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(2)),
					Containers: []corev1.Container{{
						Name:    "main",
						Image:   image,
						Command: []string{"sh", "-c", "sleep 3600"},
					}},
				},
			},
		},
	}
	created, err := cs.AppsV1().DaemonSets(ns).Create(context.Background(), ds, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create daemonset: %v", err)
	}

	// Let it converge for real first.
	res, err := settle.Run(context.Background(), cs,
		settle.Target{Kind: settle.KindDaemonSet, Namespace: ns, Name: "agent"},
		settle.Options{Timeout: 60 * time.Second, StableFor: 5 * time.Second})
	if err != nil || res.Outcome != settle.OutcomeSettled {
		t.Fatalf("baseline settle: err=%v outcome=%s (%s)", err, res.Outcome, res.Reason)
	}

	// Plant the orphan: newest revision, hash that matches nothing.
	orphan := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "agent-orphan",
			Labels: map[string]string{"controller-revision-hash": "deadbeef", "app": "agent"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: "agent",
				UID: created.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Revision: 999,
		Data:     runtime.RawExtension{Raw: []byte(`{"spec":{"template":{"metadata":{"labels":{"app":"agent"}}}}}`)},
	}
	if _, err := cs.AppsV1().ControllerRevisions(ns).Create(context.Background(), orphan, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create orphan controllerrevision: %v", err)
	}

	res, err = settle.Run(context.Background(), cs,
		settle.Target{Kind: settle.KindDaemonSet, Namespace: ns, Name: "agent"},
		settle.Options{Timeout: 60 * time.Second, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run with orphan: %v", err)
	}
	t.Logf("with orphan: outcome=%s elapsed=%s reason=%q", res.Outcome, res.Elapsed.Round(time.Second), res.Reason)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("orphan revision must not hold a converged DaemonSet at progressing, got %s (%s)", res.Outcome, res.Reason)
	}
}
