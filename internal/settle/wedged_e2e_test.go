//go:build e2e

package settle_test

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

func runSettleWedged(t *testing.T, cs *kubernetes.Clientset, ns, name string, opts settle.Options) settle.Result {
	t.Helper()
	res, err := settle.Run(context.Background(), cs,
		settle.Target{Kind: settle.KindDeployment, Namespace: ns, Name: name}, opts)
	if err != nil {
		t.Fatalf("settle.Run: %v", err)
	}
	t.Logf("outcome=%s elapsed=%s reason=%q", res.Outcome, res.Elapsed.Round(time.Second), res.Reason)
	return res
}

// A crash loop is the hard case for the wedged window: the pod flickers
// between CrashLoopBackOff and a brief running attempt, so the clock must
// accumulate across the gaps. With a long timeout the verdict must arrive
// from the window, not the deadline.
func TestWedgedForFailsCrashLoopEarly(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("wedge", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "exit 1"}
	}))

	timeout := 4 * time.Minute
	res := runSettleWedged(t, cs, ns, "wedge", settle.Options{
		Timeout: timeout, StableFor: 5 * time.Second, WedgedFor: 20 * time.Second,
	})
	if res.Outcome != settle.OutcomeFailed {
		t.Fatalf("want failed, got %s (%s)", res.Outcome, res.Reason)
	}
	if !res.WedgedOut {
		t.Fatalf("verdict must be marked as a wedged-for early exit, reason=%q", res.Reason)
	}
	// The window fires once ~20s of backoff has accumulated — around a
	// minute of crash-looping. Anywhere clearly before the deadline proves
	// the early path; the generous bound absorbs CI load.
	if res.Elapsed >= timeout-30*time.Second {
		t.Fatalf("verdict took %s of a %s timeout: the wedged window did not fire early", res.Elapsed, timeout)
	}
}

// The other side of the line: a wedge whose cause is fixed before the window
// fills must recover and settle. The deployment starts with its ConfigMap
// missing (CreateContainerConfigError, a wedge state); the ConfigMap arrives
// seconds later, the kubelet retries, the pod goes Ready, and readiness
// wipes the wedged clock.
func TestWedgedForRecoveryBeforeWindowSettles(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("mend", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
			Name: "TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "mend-config"},
					Key:                  "token",
				},
			},
		}}
	}))

	// Fix the cause while the watch is running, well inside the window.
	go func() {
		time.Sleep(5 * time.Second)
		_, err := cs.CoreV1().ConfigMaps(ns).Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "mend-config"},
			Data:       map[string]string{"token": "sesame"},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Errorf("create configmap: %v", err)
		}
	}()

	res := runSettleWedged(t, cs, ns, "mend", settle.Options{
		Timeout: 5 * time.Minute, StableFor: 5 * time.Second, WedgedFor: 90 * time.Second,
	})
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("a failure fixed inside the window must settle, got %s (%s)", res.Outcome, res.Reason)
	}
}
