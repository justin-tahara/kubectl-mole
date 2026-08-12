//go:build e2e

package settle_test

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

// The wild shape behind issue #8: paired DaemonSets sharing one label
// selector (the linux/windows agent pattern), where the second schedules to
// no nodes at all. Its verdict must not count the sibling's pods as its own
// previous revisions — before ownership filtering, it sat at progressing
// for its whole timeout on a healthy cluster.
func TestSiblingDaemonSetSharedSelector(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)

	shared := map[string]string{"k8s-app": "agent"}
	daemonset := func(name string, nodeSelector map[string]string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: shared},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: shared},
					Spec: corev1.PodSpec{
						NodeSelector:                  nodeSelector,
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
	}

	for _, ds := range []*appsv1.DaemonSet{
		daemonset("agent", nil),
		daemonset("agent-windows", map[string]string{"kubernetes.io/os": "windows"}),
	} {
		if _, err := cs.AppsV1().DaemonSets(ns).Create(context.Background(), ds, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create daemonset %s: %v", ds.Name, err)
		}
	}

	// The empty sibling first: its verdict is the regression. It must
	// settle even while the linux twin's pods match its selector.
	res, err := settle.Run(context.Background(), cs,
		settle.Target{Kind: settle.KindDaemonSet, Namespace: ns, Name: "agent-windows"},
		settle.Options{Timeout: 60 * time.Second, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run agent-windows: %v", err)
	}
	t.Logf("agent-windows: outcome=%s elapsed=%s reason=%q", res.Outcome, res.Elapsed.Round(time.Second), res.Reason)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("empty sibling must settle, got %s (%s)", res.Outcome, res.Reason)
	}

	// And the twin with real pods still settles on its own.
	res, err = settle.Run(context.Background(), cs,
		settle.Target{Kind: settle.KindDaemonSet, Namespace: ns, Name: "agent"},
		settle.Options{Timeout: 60 * time.Second, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run agent: %v", err)
	}
	t.Logf("agent: outcome=%s elapsed=%s reason=%q", res.Outcome, res.Elapsed.Round(time.Second), res.Reason)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("the populated twin must settle, got %s (%s)", res.Outcome, res.Reason)
	}
}
