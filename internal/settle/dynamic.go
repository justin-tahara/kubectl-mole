package settle

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"

	"github.com/justin-tahara/kubectl-mole/internal/perf"
)

// RunCustom watches a resource mole has no typed support for — usually a
// custom resource behind an operator — until it settles by kstatus
// conventions (Ready condition, observedGeneration). Pods the resource owns,
// directly or through a ReplicaSet, are tracked so pod-level diagnosis
// works underneath the operator.
func RunCustom(parent context.Context, cs kubernetes.Interface, dyn dynamic.Interface, gvr schema.GroupVersionResource, kind, namespace, name string, opts Options) (Result, error) {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	start := time.Now()
	target := Target{Kind: Kind(kind), Namespace: namespace, Name: name}

	pctx, pcancel := context.WithTimeout(parent, opts.Timeout)
	stopPreflight := perf.Phase("preflight")
	err := preflightCustom(pctx, cs, dyn, gvr, target)
	stopPreflight()
	pcancel()
	if err != nil {
		return Result{}, err
	}

	dfac := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, namespace, nil)
	lister := dfac.ForResource(gvr).Lister()
	tfac := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(namespace))
	src := &dynamicSource{
		target:      target,
		lister:      lister,
		pods:        tfac.Core().V1().Pods().Lister(),
		replicaSets: tfac.Apps().V1().ReplicaSets().Lister(),
	}
	// Shutdown blocks until the informer goroutines exit, and they exit on
	// context cancel — cancel (registered later, LIFO) must run first.
	defer dfac.Shutdown()
	defer tfac.Shutdown()
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	stopSync := perf.Phase("sync")
	dfac.Start(ctx.Done())
	tfac.Start(ctx.Done())
	for gvrSynced, ok := range dfac.WaitForCacheSync(ctx.Done()) {
		if !ok {
			stopSync()
			return Result{}, fmt.Errorf("timed out syncing the %s informer cache", gvrSynced.Resource)
		}
	}
	for _, ok := range tfac.WaitForCacheSync(ctx.Done()) {
		if !ok {
			stopSync()
			return Result{}, fmt.Errorf("timed out syncing informer caches for %s", target)
		}
	}
	stopSync()

	return watchLoop(parent, ctx, src.snapshot, opts, start)
}

// preflightCustom mirrors preflight for the dynamic path: informers swallow
// list errors, so a denial must surface as an immediate typed error.
func preflightCustom(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, gvr schema.GroupVersionResource, target Target) error {
	_, err := dyn.Resource(gvr).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &NotFoundError{Target: target}
		}
		if apierrors.IsForbidden(err) {
			return &PermissionError{Verb: "get", Resource: gvr.Resource, Namespace: target.Namespace}
		}
		return err
	}
	for _, r := range []string{"pods", "replicasets"} {
		if err := preflightList(ctx, cs, target.Namespace, r); err != nil {
			return err
		}
	}
	return nil
}

type dynamicSource struct {
	target      Target
	lister      cache.GenericLister
	pods        corelisters.PodLister
	replicaSets appslisters.ReplicaSetLister
}

func (s *dynamicSource) snapshot() (snapshot, error) {
	obj, err := s.lister.ByNamespace(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return snapshot{}, fmt.Errorf("unexpected object type %T for %s", obj, s.target)
	}
	res, err := status.Compute(u)
	if err != nil {
		return snapshot{}, fmt.Errorf("kstatus compute: %w", err)
	}
	pods, err := s.ownedPods(u)
	if err != nil {
		return snapshot{}, err
	}
	snap := snapshot{
		found:       true,
		kstatus:     kstatusResult{Status: res.Status, Message: res.Message},
		currentPods: pods,
	}
	// Honesty over optimism: kstatus calls a resource with no status at all
	// Current. Settling is still right — there is nothing to wait for — but
	// the verdict must say what "settled" meant.
	if _, hasStatus, _ := unstructured.NestedMap(u.Object, "status"); !hasStatus {
		snap.note = "the resource reports no status; settled means it exists"
	}
	return snap, nil
}

// ownedPods finds the pods below the resource: by its spec.selector when it
// declares one (the workload-API convention many operators follow), else by
// ownership — pods it controls directly, or through a ReplicaSet it
// controls.
func (s *dynamicSource) ownedPods(u *unstructured.Unstructured) ([]*corev1.Pod, error) {
	if sel, found, _ := unstructured.NestedStringMap(u.Object, "spec", "selector", "matchLabels"); found && len(sel) > 0 {
		pods, err := s.pods.Pods(s.target.Namespace).List(labels.SelectorFromSet(sel))
		if err != nil {
			return nil, err
		}
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		return pods, nil
	}

	all, err := s.pods.Pods(s.target.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	rsOwned := func(rsName string) bool {
		rs, err := s.replicaSets.ReplicaSets(s.target.Namespace).Get(rsName)
		if err != nil {
			return false
		}
		ref := metav1.GetControllerOf(rs)
		return ref != nil && ref.UID == u.GetUID()
	}
	var out []*corev1.Pod
	for _, p := range all {
		ref := metav1.GetControllerOf(p)
		if ref == nil {
			continue
		}
		if ref.UID == u.GetUID() || (ref.Kind == "ReplicaSet" && rsOwned(ref.Name)) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
