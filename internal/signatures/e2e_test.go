//go:build e2e

// End-to-end signature scenarios against a disposable cluster: each one
// reproduces a real failure and asserts the catalogue names it. Gated on
// MOLE_E2E_CONTEXT exactly like the settle e2e tests. AdmissionRejected has
// no scenario here (it needs a webhook server) and is covered by unit tests.
package signatures_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/collapse"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

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
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mole-sig-"}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return ns.Name
}

func newDeployment(name string, mut ...func(*appsv1.Deployment)) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
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

// watchAndDiagnose runs the settle engine (asserting the workload does not
// settle), then polls Diagnose until the wanted signature appears and every
// accept predicate holds — pod status and log availability flap between
// backoff states, so a single instant can miss either.
func watchAndDiagnose(t *testing.T, cs *kubernetes.Clientset, ns, name, wantSignature string, timeout time.Duration, accept ...func(signatures.Finding) bool) signatures.Finding {
	t.Helper()
	target := settle.Target{Kind: settle.KindDeployment, Namespace: ns, Name: name}
	res, err := settle.Run(context.Background(), cs, target,
		settle.Options{Timeout: timeout, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run: %v", err)
	}
	t.Logf("outcome=%s reason=%q", res.Outcome, res.Reason)
	if res.Outcome == settle.OutcomeSettled {
		t.Fatalf("scenario unexpectedly settled")
	}

	ref := signatures.TargetRef{Kind: "Deployment", Namespace: ns, Name: name}
	deadline := time.Now().Add(30 * time.Second)
	var last signatures.Report
	for {
		pods := listPods(t, cs, ns, name)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		last = signatures.Diagnose(ctx, cs, ref, pods, nil)
		cancel()
	scan:
		for _, f := range last.Findings {
			if f.Signature != wantSignature {
				continue
			}
			for _, ok := range accept {
				if !ok(f) {
					continue scan
				}
			}
			t.Logf("finding: %s: %s (chain %v)", f.Signature, f.Cause, f.Chain)
			return f
		}
		if time.Now().After(deadline) {
			var states []string
			for _, p := range pods {
				for _, cs := range p.Status.ContainerStatuses {
					states = append(states, fmt.Sprintf("%s/%s state=%+v last=%+v restarts=%d",
						p.Name, cs.Name, cs.State, cs.LastTerminationState, cs.RestartCount))
				}
			}
			t.Fatalf("no %s finding before deadline\npod states: %s\nlast report: %+v",
				wantSignature, strings.Join(states, "; "), last)
		}
		time.Sleep(2 * time.Second)
	}
}

func listPods(t *testing.T, cs *kubernetes.Clientset, ns, app string) []*corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
	}
	return pods
}

func TestE2EImagePull(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("pull", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Image = "ghcr.io/justin-tahara/no-such-image:v1"
		d.Spec.Template.Spec.Containers[0].Command = nil
	}))

	f := watchAndDiagnose(t, cs, ns, "pull", "ImagePullBackOff", 30*time.Second)
	if !strings.Contains(f.Cause, "no-such-image") {
		t.Fatalf("cause should name the image, got %q", f.Cause)
	}
	if len(f.Chain) != 3 || !strings.HasPrefix(f.Chain[1], "ReplicaSet/") {
		t.Fatalf("chain should walk Deployment -> ReplicaSet -> Pod, got %v", f.Chain)
	}
	if len(f.Evidence) == 0 {
		t.Fatal("expected pull-error evidence")
	}
}

func TestE2ECrashLoopWithLogEvidence(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("crash", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "echo MOLE-LOG-MARKER; sleep 1; exit 7"}
	}))

	// Log availability flaps during restart transitions: poll until the
	// finding carries the previous container's log evidence.
	f := watchAndDiagnose(t, cs, ns, "crash", "CrashLoopBackOff", 40*time.Second, func(f signatures.Finding) bool {
		for _, ev := range f.Evidence {
			if ev.Source == "log" && strings.Contains(ev.Text, "MOLE-LOG-MARKER") {
				return true
			}
		}
		return false
	})
	if !strings.Contains(f.Cause, "exit code 7") {
		t.Fatalf("cause should carry the exit code, got %q", f.Cause)
	}
}

func TestE2EOOMKilled(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("oom", func(d *appsv1.Deployment) {
		c := &d.Spec.Template.Spec.Containers[0]
		c.Command = []string{"sh", "-c", "sleep 2; tail /dev/zero"}
		c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}
		c.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}
	}))

	f := watchAndDiagnose(t, cs, ns, "oom", "OOMKilled", 40*time.Second)
	if !strings.Contains(f.Cause, "16Mi") {
		t.Fatalf("cause should carry the memory limit, got %q", f.Cause)
	}
}

func TestE2EUnschedulable(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("big", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("64"),
		}
	}))

	f := watchAndDiagnose(t, cs, ns, "big", "PodUnschedulable", 20*time.Second)
	if !strings.Contains(f.Cause, "Insufficient cpu") {
		t.Fatalf("cause should carry the scheduler predicate, got %q", f.Cause)
	}
}

func TestE2EQuotaExceeded(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	_, err := cs.CoreV1().ResourceQuotas(ns).Create(context.Background(), &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "no-pods"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("0")}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create quota: %v", err)
	}
	create(t, cs, ns, newDeployment("quota"))

	f := watchAndDiagnose(t, cs, ns, "quota", "QuotaExceeded", 20*time.Second)
	if !strings.Contains(f.Cause, "no-pods") {
		t.Fatalf("cause should name the quota, got %q", f.Cause)
	}
	if len(f.Chain) != 1 || f.Chain[0] != "Deployment/quota" {
		t.Fatalf("workload-level finding should chain to the workload only, got %v", f.Chain)
	}
}

func TestE2EPVCPending(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	_, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("no-such-class"),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pvc: %v", err)
	}
	create(t, cs, ns, newDeployment("vol", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
		}}
	}))

	f := watchAndDiagnose(t, cs, ns, "vol", "PVCPending", 20*time.Second)
	if !strings.Contains(f.Cause, `"data"`) || !strings.Contains(f.Cause, "no-such-class") {
		t.Fatalf("cause should name the PVC and storageClass, got %q", f.Cause)
	}
}

func create(t *testing.T, cs *kubernetes.Clientset, ns string, d *appsv1.Deployment) {
	t.Helper()
	if _, err := cs.AppsV1().Deployments(ns).Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
}

// TestE2EIdenticalCausesCollapse reproduces the dedup mandate live: three
// replicas crashing for the same reason must fold into one entry with
// affected: 3, not three findings a consumer would chase separately.
func TestE2EIdenticalCausesCollapse(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("crashtrio", func(d *appsv1.Deployment) {
		d.Spec.Replicas = ptr.To(int32(3))
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 1; exit 7"}
	}))

	target := settle.Target{Kind: settle.KindDeployment, Namespace: ns, Name: "crashtrio"}
	res, err := settle.Run(context.Background(), cs, target,
		settle.Options{Timeout: 40 * time.Second, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run: %v", err)
	}
	if res.Outcome == settle.OutcomeSettled {
		t.Fatal("scenario unexpectedly settled")
	}

	// Pods reach the exit-code-bearing cause at different times; poll until
	// all three share it and collapse to a single entry.
	ref := signatures.TargetRef{Kind: "Deployment", Namespace: ns, Name: "crashtrio"}
	deadline := time.Now().Add(60 * time.Second)
	var entries []collapse.Entry
	for {
		pods := listPods(t, cs, ns, "crashtrio")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		rep := signatures.Diagnose(ctx, cs, ref, pods, nil)
		cancel()
		entries = collapse.Collapse(rep.Findings)
		if len(entries) == 1 && entries[0].Signature == "CrashLoopBackOff" && entries[0].Affected == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no single collapsed entry with affected=3 before deadline; entries: %+v", entries)
		}
		time.Sleep(2 * time.Second)
	}
	e := entries[0]
	if len(e.Examples) != 3 || len(e.Pods) != 3 {
		t.Fatalf("want 3 examples and 3 pods, got %+v", e)
	}
	for _, ex := range e.Examples {
		if !strings.HasPrefix(ex, ns+"/crashtrio-") {
			t.Fatalf("examples must be namespace-qualified pod refs, got %v", e.Examples)
		}
	}
}

func TestE2EConfigMissing(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("cfg", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
			Name: "DB_HOST",
			ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-config"},
				Key:                  "host",
			}},
		}}
	}))

	f := watchAndDiagnose(t, cs, ns, "cfg", "ConfigMissing", 30*time.Second)
	if !strings.Contains(f.Cause, "missing-config") {
		t.Fatalf("cause should name the missing ConfigMap, got %q", f.Cause)
	}
}

func TestE2EStartFailed(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("noexec", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Command = []string{"/no-such-binary"}
	}))

	f := watchAndDiagnose(t, cs, ns, "noexec", "ContainerStartFailed", 40*time.Second)
	if !strings.Contains(f.Cause, "no-such-binary") && !strings.Contains(f.Cause, "executable file not found") {
		t.Fatalf("cause should carry the runtime's exec error, got %q", f.Cause)
	}
}

func TestE2EVolumeMountFailed(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("mnt", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "creds",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "missing-secret"}},
		}}
		d.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "creds", MountPath: "/creds"}}
	}))

	f := watchAndDiagnose(t, cs, ns, "mnt", "VolumeMountFailed", 45*time.Second)
	if !strings.Contains(f.Cause, "missing-secret") {
		t.Fatalf("cause should name the missing Secret, got %q", f.Cause)
	}
}

func TestE2EEvicted(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("hog", func(d *appsv1.Deployment) {
		c := &d.Spec.Template.Spec.Containers[0]
		// Exceed the pod's own ephemeral-storage limit; the kubelet's
		// eviction manager notices within its ~10s housekeeping interval.
		c.Command = []string{"sh", "-c", "dd if=/dev/zero of=/tmp/fill bs=1M count=50; sleep 3600"}
		c.Resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("5Mi")}
	}))

	f := watchAndDiagnose(t, cs, ns, "hog", "PodEvicted", 90*time.Second)
	if !strings.Contains(strings.ToLower(f.Cause), "ephemeral") {
		t.Fatalf("cause should name the storage limit breach, got %q", f.Cause)
	}
}

// TestE2EStuckTerminatingOldPod wedges a rollout the way operators meet it:
// the old pod carries a finalizer, the new revision comes up fine, and the
// deployment can never finish. The finding must come from the old-pod path.
func TestE2EStuckTerminatingOldPod(t *testing.T) {
	cs := client(t)
	ns := testNamespace(t, cs)
	create(t, cs, ns, newDeployment("wedge"))

	// Wait for the first revision to come up, then pin its pod.
	target := settle.Target{Kind: settle.KindDeployment, Namespace: ns, Name: "wedge"}
	res, err := settle.Run(context.Background(), cs, target, settle.Options{Timeout: 60 * time.Second, StableFor: 3 * time.Second})
	if err != nil || res.Outcome != settle.OutcomeSettled {
		t.Fatalf("first revision should settle: outcome=%v err=%v", res.Outcome, err)
	}
	pods := listPods(t, cs, ns, "wedge")
	if len(pods) != 1 {
		t.Fatalf("want one pod, got %d", len(pods))
	}
	pinned := pods[0].Name
	patch := []byte(`{"metadata":{"finalizers":["mole.example/e2e-block"]}}`)
	if _, err := cs.CoreV1().Pods(ns).Patch(context.Background(), pinned, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("pin pod with finalizer: %v", err)
	}
	// Strip the finalizer no matter how the test ends — the namespace cannot
	// delete while the pod is pinned.
	t.Cleanup(func() {
		unpatch := []byte(`{"metadata":{"finalizers":[]}}`)
		_, _ = cs.CoreV1().Pods(ns).Patch(context.Background(), pinned, types.StrategicMergePatchType, unpatch, metav1.PatchOptions{})
	})

	// Roll to a new revision; the pinned old pod wedges it.
	dep, err := cs.AppsV1().Deployments(ns).Get(context.Background(), "wedge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	dep.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "REV", Value: "2"}}
	if _, err := cs.AppsV1().Deployments(ns).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update deployment: %v", err)
	}

	// The rollout must not settle (grace 2s + 30s slack before the detector
	// fires, so give the watch room).
	res, err = settle.Run(context.Background(), cs, target, settle.Options{Timeout: 75 * time.Second, StableFor: 3 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run: %v", err)
	}
	if res.Outcome == settle.OutcomeSettled {
		t.Fatal("rollout settled despite the pinned old pod")
	}
	if len(res.Final.OldPods) == 0 {
		t.Fatalf("the pinned pod should surface as an old pod, got %+v", res.Final)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rep := signatures.Diagnose(ctx, cs, signatures.TargetRef{Kind: "Deployment", Namespace: ns, Name: "wedge"},
		res.Final.CurrentPods, res.Final.OldPods)
	for _, f := range rep.Findings {
		if f.Signature == "PodStuckTerminating" && f.Pod == pinned && strings.Contains(f.Cause, "mole.example/e2e-block") {
			t.Logf("finding: %s: %s (chain %v)", f.Signature, f.Cause, f.Chain)
			return
		}
	}
	t.Fatalf("no PodStuckTerminating finding for the pinned old pod; findings: %+v", rep.Findings)
}
