//go:build e2e

package settle_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/justin-tahara/kubectl-mole/internal/collapse"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

func podsOf(t *testing.T, cs *kubernetes.Clientset, ns, app string) []*corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods
}

// TestFleetAcrossNamespaces is the M6 guard: one fan-out over two namespaces
// watches everything the selector matches off one shared informer set,
// reports each target's own outcome, keeps deterministic order, and folds
// identical causes from different namespaces into a single entry.
func TestFleetAcrossNamespaces(t *testing.T) {
	cs := client(t)
	ns1 := testNamespace(t, cs)
	ns2 := testNamespace(t, cs)
	// The generated namespace name doubles as a run-unique label value, so
	// concurrent test runs cannot see each other's workloads.
	selector := "mole-e2e-fleet=" + ns1
	tag := func(d *appsv1.Deployment) {
		d.ObjectMeta.Labels = map[string]string{"mole-e2e-fleet": ns1}
	}
	crash := func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 1; exit 7"}
	}
	create(t, cs, ns1, newDeployment("web", 1, tag))
	create(t, cs, ns1, newDeployment("api", 1, tag, crash))
	create(t, cs, ns1, newDeployment("bystander", 1))
	create(t, cs, ns2, newDeployment("api", 1, tag, crash))

	results, err := settle.RunFleet(context.Background(), cs, settle.Scope{Selector: selector},
		settle.Options{Timeout: 45 * time.Second, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("RunFleet: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("selector must pick exactly the 3 labeled workloads, got %d: %+v", len(results), results)
	}
	outcomes := map[string]settle.Outcome{}
	for _, r := range results {
		outcomes[r.Target.Namespace+"/"+r.Target.Name] = r.Result.Outcome
		t.Logf("%s/%s: %s (%s)", r.Target.Namespace, r.Target.Name, r.Result.Outcome, r.Result.Reason)
	}
	if outcomes[ns1+"/web"] != settle.OutcomeSettled {
		t.Fatalf("healthy target must settle, got %v", outcomes)
	}
	if outcomes[ns1+"/api"] != settle.OutcomeFailed || outcomes[ns2+"/api"] != settle.OutcomeFailed {
		t.Fatalf("crashing targets must fail, got %v", outcomes)
	}
	for i := 1; i < len(results); i++ {
		a, b := results[i-1].Target, results[i].Target
		if a.Namespace > b.Namespace || (a.Namespace == b.Namespace && a.Name > b.Name) {
			t.Fatalf("results out of order: %v before %v", a, b)
		}
	}

	// The same cause in two namespaces must collapse to one entry with
	// namespace-qualified anchors from both. Pods reach the
	// exit-code-bearing cause at different times; poll until they agree.
	deadline := time.Now().Add(60 * time.Second)
	var entries []collapse.Entry
	for {
		var findings []signatures.Finding
		for _, ns := range []string{ns1, ns2} {
			dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
			rep := signatures.Diagnose(dctx, cs,
				signatures.TargetRef{Kind: "Deployment", Namespace: ns, Name: "api"}, podsOf(t, cs, ns, "api"))
			dcancel()
			findings = append(findings, rep.Findings...)
		}
		entries = collapse.Collapse(findings)
		if len(entries) == 1 && entries[0].Signature == "CrashLoopBackOff" && entries[0].Affected == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no single cross-namespace entry with affected=2 before deadline; entries: %+v", entries)
		}
		time.Sleep(2 * time.Second)
	}
	spans := map[string]bool{}
	for _, ex := range entries[0].Examples {
		for _, ns := range []string{ns1, ns2} {
			if strings.HasPrefix(ex, ns+"/") {
				spans[ns] = true
			}
		}
	}
	if !spans[ns1] || !spans[ns2] {
		t.Fatalf("examples must span both namespaces, got %v", entries[0].Examples)
	}

	// The ceiling refuses the same selection when set below the match count,
	// before any watch starts.
	_, err = settle.RunFleet(context.Background(), cs, settle.Scope{Selector: selector, MaxTargets: 1},
		settle.Options{Timeout: 10 * time.Second, StableFor: time.Second})
	var oc *settle.OverCeilingError
	if !errors.As(err, &oc) {
		t.Fatalf("want OverCeilingError below the match count, got %v", err)
	}
}
