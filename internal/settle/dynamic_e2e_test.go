//go:build e2e

// End-to-end settle semantics for custom resources: kstatus conventions,
// honest no-status verdicts, and pod diagnosis underneath an operator.
package settle_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

var (
	widgetGVR = schema.GroupVersionResource{Group: "mole.example.com", Version: "v1", Resource: "widgets"}
	crdGVR    = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

func dynClient(t *testing.T) dynamic.Interface {
	t.Helper()
	ctxName := os.Getenv("MOLE_E2E_CONTEXT")
	if ctxName == "" {
		t.Skip("MOLE_E2E_CONTEXT not set")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build dynamic client: %v", err)
	}
	return dyn
}

// ensureWidgetCRD installs the test CRD (idempotent) and waits for it to be
// established. The CRD is cluster-scoped and shared; it is left in place —
// the cluster is disposable and parallel tests may still be using it.
func ensureWidgetCRD(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "widgets.mole.example.com"},
		"spec": map[string]any{
			"group": "mole.example.com",
			"names": map[string]any{"plural": "widgets", "singular": "widget", "kind": "Widget"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"subresources": map[string]any{"status": map[string]any{}},
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type":                                 "object",
					"x-kubernetes-preserve-unknown-fields": true,
				}},
			}},
		},
	}}
	_, err := dyn.Resource(crdGVR).Create(context.Background(), crd, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create widget CRD: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := dyn.Resource(crdGVR).Get(context.Background(), "widgets.mole.example.com", metav1.GetOptions{})
		if err == nil {
			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			for _, c := range conds {
				m, _ := c.(map[string]any)
				if m["type"] == "Established" && m["status"] == "True" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("widget CRD not established: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func widget(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "mole.example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{"size": int64(1)},
	}}
}

func setWidgetReady(t *testing.T, dyn dynamic.Interface, ns, name string, ready bool, msg string) {
	t.Helper()
	got, err := dyn.Resource(widgetGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get widget: %v", err)
	}
	st := "False"
	if ready {
		st = "True"
	}
	got.Object["status"] = map[string]any{
		"observedGeneration": got.GetGeneration(),
		"conditions": []any{map[string]any{
			"type": "Ready", "status": st, "reason": "Test", "message": msg,
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		}},
	}
	if _, err := dyn.Resource(widgetGVR).Namespace(ns).UpdateStatus(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update widget status: %v", err)
	}
}

func runCustom(t *testing.T, cs *kubernetes.Clientset, dyn dynamic.Interface, ns, name string, timeout time.Duration) settle.Result {
	t.Helper()
	res, err := settle.RunCustom(context.Background(), cs, dyn, widgetGVR, "Widget", ns, name,
		settle.Options{Timeout: timeout, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.RunCustom: %v", err)
	}
	t.Logf("outcome=%s reason=%q", res.Outcome, res.Reason)
	return res
}

func TestCustomResource(t *testing.T) {
	t.Parallel()
	cs := client(t)
	dyn := dynClient(t)
	ensureWidgetCRD(t, dyn)
	ns := testNamespace(t, cs)

	mkWidget := func(name string) {
		if _, err := dyn.Resource(widgetGVR).Namespace(ns).Create(context.Background(), widget(ns, name), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create widget %s: %v", name, err)
		}
	}

	t.Run("ready settles", func(t *testing.T) {
		mkWidget("good")
		setWidgetReady(t, dyn, ns, "good", true, "all good")
		res := runCustom(t, cs, dyn, ns, "good", 45*time.Second)
		if res.Outcome != settle.OutcomeSettled {
			t.Fatalf("Ready widget must settle, got %s (%s)", res.Outcome, res.Reason)
		}
	})

	t.Run("no status settles honestly", func(t *testing.T) {
		mkWidget("mute")
		res := runCustom(t, cs, dyn, ns, "mute", 45*time.Second)
		if res.Outcome != settle.OutcomeSettled {
			t.Fatalf("a status-less resource settles by existing, got %s (%s)", res.Outcome, res.Reason)
		}
		if !strings.Contains(res.Reason, "no status") {
			t.Fatalf("the verdict must say what settled meant, got %q", res.Reason)
		}
	})

	t.Run("not ready is progressing", func(t *testing.T) {
		mkWidget("sad")
		setWidgetReady(t, dyn, ns, "sad", false, "widget is warming up")
		res := runCustom(t, cs, dyn, ns, "sad", 15*time.Second)
		if res.Outcome != settle.OutcomeProgressing {
			t.Fatalf("Ready=False is progressing at timeout, got %s (%s)", res.Outcome, res.Reason)
		}
	})

	t.Run("owned pod is diagnosed", func(t *testing.T) {
		mkWidget("boss")
		setWidgetReady(t, dyn, ns, "boss", true, "all good")
		owner, err := dyn.Resource(widgetGVR).Namespace(ns).Get(context.Background(), "boss", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get widget: %v", err)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "boss-worker",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "mole.example.com/v1", Kind: "Widget",
					Name: owner.GetName(), UID: owner.GetUID(), Controller: ptr.To(true),
				}},
			},
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: ptr.To(int64(2)),
				Containers: []corev1.Container{{
					Name: "main", Image: image,
					Command: []string{"sh", "-c", "sleep 1; exit 7"},
				}},
			},
		}
		if _, err := cs.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create owned pod: %v", err)
		}

		// The widget says Ready, but its pod crash-loops: the pod-level check
		// blocks settle and the timeout verdict is failed.
		res := runCustom(t, cs, dyn, ns, "boss", 45*time.Second)
		if res.Outcome != settle.OutcomeFailed {
			t.Fatalf("a crash-looping owned pod must fail the watch, got %s (%s)", res.Outcome, res.Reason)
		}

		// The crash loop flickers through restart attempts, and a pod sampled
		// mid-attempt shows Running — no waiting reason, no finding. Diagnose
		// against a FRESH pod read until the backoff state is visible again;
		// retrying the stale watch-end snapshot would never converge.
		deadline := time.Now().Add(60 * time.Second)
		var findings []signatures.Finding
		for {
			pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), "boss-worker", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("refetch owned pod: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			rep := signatures.Diagnose(ctx, cs, signatures.TargetRef{Kind: "Widget", Namespace: ns, Name: "boss"},
				[]*corev1.Pod{pod}, nil)
			cancel()
			findings = rep.Findings
			for _, f := range findings {
				if f.Signature == "CrashLoopBackOff" && f.Chain[0] == "Widget/boss" {
					t.Logf("finding: %s: %s (chain %v)", f.Signature, f.Cause, f.Chain)
					return
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("no CrashLoopBackOff finding chained to the widget; findings: %+v", findings)
	})
}
