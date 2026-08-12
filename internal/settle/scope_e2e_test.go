//go:build e2e

package settle_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

// The scoped informer caches must not change what the verdict sees: a
// crash-looping pod with foreign labels in the same namespace belongs to
// somebody else's workload, and the target must settle regardless of it —
// the shared-namespace case the cache scoping exists for.
func TestSharedNamespaceDecoyIgnored(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)

	decoy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decoy-crasher", Labels: map[string]string{"app": "decoy"}},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To(int64(2)),
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   image,
				Command: []string{"sh", "-c", "exit 1"},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(context.Background(), decoy, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create decoy pod: %v", err)
	}
	create(t, cs, ns, newDeployment("app", 1))

	res := runSettle(t, cs, ns, "app", 60*time.Second, 5*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("a crashing decoy in the namespace must not affect the verdict, got %s (%s)", res.Outcome, res.Reason)
	}
}
