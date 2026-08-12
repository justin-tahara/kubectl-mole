package settle

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func labeled(labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Labels: labels}
}

func deployment(ns, name string, labels map[string]string) *appsv1.Deployment {
	m := labeled(labels)
	m.Namespace, m.Name = ns, name
	return &appsv1.Deployment{ObjectMeta: m}
}

func statefulSet(ns, name string, labels map[string]string) *appsv1.StatefulSet {
	m := labeled(labels)
	m.Namespace, m.Name = ns, name
	return &appsv1.StatefulSet{ObjectMeta: m}
}

func daemonSet(ns, name string, labels map[string]string) *appsv1.DaemonSet {
	m := labeled(labels)
	m.Namespace, m.Name = ns, name
	return &appsv1.DaemonSet{ObjectMeta: m}
}

// TestDiscoverFiltersAndSorts proves selection is by label and the result
// order is (namespace, kind, name) regardless of listing order.
func TestDiscoverFiltersAndSorts(t *testing.T) {
	sel := map[string]string{"part-of": "platform"}
	cs := fake.NewClientset(
		deployment("b", "api", sel),
		deployment("a", "web", sel),
		deployment("a", "other", map[string]string{"part-of": "elsewhere"}),
		statefulSet("a", "db", sel),
		daemonSet("b", "agent", sel),
	)
	targets, err := Discover(context.Background(), cs, Scope{Selector: "part-of=platform"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []Target{
		{Kind: KindDeployment, Namespace: "a", Name: "web"},
		{Kind: KindStatefulSet, Namespace: "a", Name: "db"},
		{Kind: KindDaemonSet, Namespace: "b", Name: "agent"},
		{Kind: KindDeployment, Namespace: "b", Name: "api"},
	}
	if len(targets) != len(want) {
		t.Fatalf("got %d targets %v, want %d", len(targets), targets, len(want))
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("target %d: got %+v, want %+v", i, targets[i], want[i])
		}
	}
}

func TestDiscoverScopedToNamespace(t *testing.T) {
	cs := fake.NewClientset(
		deployment("a", "api", nil),
		deployment("b", "api", nil),
	)
	targets, err := Discover(context.Background(), cs, Scope{Namespace: "a"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(targets) != 1 || targets[0].Namespace != "a" {
		t.Fatalf("want only namespace a, got %v", targets)
	}
}

func TestDiscoverOverCeiling(t *testing.T) {
	cs := fake.NewClientset(
		deployment("a", "one", nil),
		deployment("a", "two", nil),
		deployment("a", "three", nil),
	)
	_, err := Discover(context.Background(), cs, Scope{MaxTargets: 2})
	var oc *OverCeilingError
	if !errors.As(err, &oc) {
		t.Fatalf("want OverCeilingError, got %v", err)
	}
	if oc.Ceiling != 2 || oc.Matched < 3 {
		t.Fatalf("got matched=%d ceiling=%d", oc.Matched, oc.Ceiling)
	}
	if !strings.Contains(oc.Error(), "narrow the selector") {
		t.Fatalf("refusal must say what to do instead, got %q", oc.Error())
	}
}

func TestDiscoverNoMatch(t *testing.T) {
	cs := fake.NewClientset(deployment("a", "api", nil))
	_, err := Discover(context.Background(), cs, Scope{Selector: "app=absent"})
	var nm *NoMatchError
	if !errors.As(err, &nm) {
		t.Fatalf("want NoMatchError, got %v", err)
	}
	if nm.Error() != `no workloads match selector "app=absent" in any namespace` {
		t.Fatalf("unexpected message %q", nm.Error())
	}
}

func TestDiscoverForbiddenClusterScope(t *testing.T) {
	cs := fake.NewClientset()
	forbid(cs, "list", "deployments")
	_, err := Discover(context.Background(), cs, Scope{})
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("want PermissionError, got %v", err)
	}
	if pe.Error() != "cannot list deployments at the cluster scope" {
		t.Fatalf("unexpected message %q", pe.Error())
	}
}

func TestDiscoverInvalidSelector(t *testing.T) {
	cs := fake.NewClientset()
	_, err := Discover(context.Background(), cs, Scope{Selector: "app==,"})
	if err == nil || !strings.Contains(err.Error(), "invalid selector") {
		t.Fatalf("want invalid-selector error, got %v", err)
	}
}
