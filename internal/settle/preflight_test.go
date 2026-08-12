package settle

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func forbid(cs *fake.Clientset, verb, resource string) {
	cs.PrependReactor(verb, resource, func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC denied"))
	})
}

func TestPreflightNotFound(t *testing.T) {
	cs := fake.NewClientset()
	_, err := preflight(context.Background(), cs, Target{Kind: KindDeployment, Namespace: "prod", Name: "api"})
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want NotFoundError, got %v", err)
	}
	if nf.Error() != "Deployment/api not found in namespace prod" {
		t.Fatalf("unexpected message %q", nf.Error())
	}
}

func TestPreflightForbiddenWorkload(t *testing.T) {
	cs := fake.NewClientset()
	forbid(cs, "get", "statefulsets")
	_, err := preflight(context.Background(), cs, Target{Kind: KindStatefulSet, Namespace: "prod", Name: "db"})
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("want PermissionError, got %v", err)
	}
	if pe.Error() != "cannot get statefulsets in namespace prod" {
		t.Fatalf("message must name verb and resource, got %q", pe.Error())
	}
}

func TestPreflightForbiddenPodList(t *testing.T) {
	cs := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
	})
	forbid(cs, "list", "pods")
	_, err := preflight(context.Background(), cs, Target{Kind: KindDeployment, Namespace: "prod", Name: "api"})
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("want PermissionError, got %v", err)
	}
	if pe.Verb != "list" || pe.Resource != "pods" {
		t.Fatalf("got verb %q resource %q, want list/pods", pe.Verb, pe.Resource)
	}
}

// TestRunSurfacesTypedErrors proves the typed errors reach Run's caller
// immediately, not after the watch timeout.
func TestRunSurfacesTypedErrors(t *testing.T) {
	cs := fake.NewClientset()
	start := time.Now()
	_, err := Run(context.Background(), cs, Target{Kind: KindDaemonSet, Namespace: "kube-system", Name: "gone"},
		Options{Timeout: time.Minute, StableFor: time.Second})
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want NotFoundError, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a missing target must fail fast, not wait for the timeout")
	}
}

// TestPreflightScopesTheWatch proves the fetched workload's selector becomes
// the related-object cache scope, and a bare Pod pins by name.
func TestPreflightScopesTheWatch(t *testing.T) {
	cs := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		},
	})
	scope, err := preflight(context.Background(), cs, Target{Kind: KindDeployment, Namespace: "prod", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if scope.labelSelector != "app=api" || scope.fieldSelector != "" {
		t.Fatalf("want label scope app=api, got %+v", scope)
	}

	cs = fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "prod"}})
	scope, err = preflight(context.Background(), cs, Target{Kind: KindPod, Namespace: "prod", Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if scope.fieldSelector != "metadata.name=one" || scope.labelSelector != "" {
		t.Fatalf("want name-pinned scope, got %+v", scope)
	}
}
