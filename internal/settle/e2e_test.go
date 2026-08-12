//go:build e2e

// End-to-end guard tests for settle semantics, run against a disposable
// cluster. Refuses to run unless MOLE_E2E_CONTEXT names the kube context
// explicitly (e.g. kind-mole-dev) — never the ambient current context.
package settle_test

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

// Docker Hub official image via the ECR Public mirror: no anonymous-pull rate
// limits in CI.
const image = "public.ecr.aws/docker/library/busybox:1.36"

func client(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	ctxName := os.Getenv("MOLE_E2E_CONTEXT")
	if ctxName == "" {
		t.Skip("MOLE_E2E_CONTEXT not set; set it to a disposable cluster's context (e.g. kind-mole-dev) to run e2e tests")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig for context %q: %v", ctxName, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return cs
}

func testNamespace(t *testing.T, cs *kubernetes.Clientset) string {
	t.Helper()
	ns, err := cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mole-e2e-"}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return ns.Name
}

func newDeployment(name string, replicas int32, mut ...func(*appsv1.Deployment)) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
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
	for _, m := range mut {
		m(d)
	}
	return d
}

func create(t *testing.T, cs *kubernetes.Clientset, ns string, d *appsv1.Deployment) {
	t.Helper()
	if _, err := cs.AppsV1().Deployments(ns).Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
}

func update(t *testing.T, cs *kubernetes.Clientset, ns, name string, mut func(*appsv1.Deployment)) {
	t.Helper()
	d, err := cs.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	mut(d)
	if _, err := cs.AppsV1().Deployments(ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update deployment: %v", err)
	}
}

func runSettle(t *testing.T, cs *kubernetes.Clientset, ns, name string, timeout, stableFor time.Duration) settle.Result {
	t.Helper()
	res, err := settle.Run(context.Background(), cs,
		settle.Target{Kind: settle.KindDeployment, Namespace: ns, Name: name},
		settle.Options{Timeout: timeout, StableFor: stableFor})
	if err != nil {
		t.Fatalf("settle.Run: %v", err)
	}
	t.Logf("outcome=%s elapsed=%s reason=%q", res.Outcome, res.Elapsed.Round(time.Second), res.Reason)
	return res
}

func neverReadyProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "test -f /never"}}},
		InitialDelaySeconds: 1,
		PeriodSeconds:       2,
		FailureThreshold:    1,
	}
}

// Baseline: a healthy deployment settles.
func TestHappyPathSettles(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("happy", 1))

	res := runSettle(t, cs, ns, "happy", 90*time.Second, 5*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("want settled, got %s (%s)", res.Outcome, res.Reason)
	}
}

// Guard 1 + 2: after an update whose new pods can never become ready, the old
// pod stays Ready the whole time and the workload's status initially still
// describes the old rollout. A naive check says healthy; mole must not settle,
// and must classify the timeout as progressing, not failed.
func TestRollingUpdateDoesNotSettleOnOldState(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("roll", 1))
	if res := runSettle(t, cs, ns, "roll", 90*time.Second, 5*time.Second); res.Outcome != settle.OutcomeSettled {
		t.Fatalf("baseline settle failed: %s (%s)", res.Outcome, res.Reason)
	}

	update(t, cs, ns, "roll", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].ReadinessProbe = neverReadyProbe()
	})
	res := runSettle(t, cs, ns, "roll", 25*time.Second, 5*time.Second)
	if res.Outcome == settle.OutcomeSettled {
		t.Fatalf("settled on the old rollout's state: %s", res.Reason)
	}
	if res.Outcome != settle.OutcomeProgressing {
		t.Fatalf("unready-but-clean rollout at timeout: want progressing, got %s (%s)", res.Outcome, res.Reason)
	}
}

// Guard 3: pod goes Ready, then crashes seconds later. The stability window
// must catch it, and the observed restarts make the verdict failed.
func TestStableForCatchesLateCrash(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("crash", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 8; exit 1"}
	}))

	res := runSettle(t, cs, ns, "crash", 45*time.Second, 20*time.Second)
	if res.Outcome == settle.OutcomeSettled {
		t.Fatalf("settled on a pod that crashes after going Ready: %s", res.Reason)
	}
	if res.Outcome != settle.OutcomeFailed {
		t.Fatalf("restarts were observed: want failed, got %s (%s)", res.Outcome, res.Reason)
	}
}

// Guard 4: genuinely still progressing at timeout is progressing (exit 2
// territory), never failed — automation must not roll back on it.
func TestProgressingAtTimeoutIsNotFailed(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("slow", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].ReadinessProbe = neverReadyProbe()
	}))

	res := runSettle(t, cs, ns, "slow", 20*time.Second, 5*time.Second)
	if res.Outcome != settle.OutcomeProgressing {
		t.Fatalf("want progressing, got %s (%s)", res.Outcome, res.Reason)
	}
}

// Guard 1 (termination tail): new pods ready fast, old pods linger in
// preStop for ~20s. Controllers stop counting terminating pods immediately,
// so a kstatus-only check settles early; mole must wait for the old pods to
// actually be gone.
func TestOldPodTerminationBlocksSettle(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("drain", 2, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(25))
		d.Spec.Template.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "sleep 20"}}},
		}
	}))
	if res := runSettle(t, cs, ns, "drain", 120*time.Second, 3*time.Second); res.Outcome != settle.OutcomeSettled {
		t.Fatalf("baseline settle failed: %s (%s)", res.Outcome, res.Reason)
	}

	update(t, cs, ns, "drain", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ROLL", Value: "2"}}
	})
	res := runSettle(t, cs, ns, "drain", 120*time.Second, 3*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("want settled after old pods drained, got %s (%s)", res.Outcome, res.Reason)
	}
	// Old pods hold preStop for 20s; settling much earlier means the engine
	// ignored them.
	if res.Elapsed < 12*time.Second {
		t.Fatalf("settled in %s — old terminating pods were ignored", res.Elapsed.Round(time.Second))
	}
}
