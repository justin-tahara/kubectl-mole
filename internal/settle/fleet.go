package settle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/justin-tahara/kubectl-mole/internal/perf"
)

// DefaultMaxTargets is the fleet-size ceiling when the caller does not set
// one.
const DefaultMaxTargets = 5000

// Scope selects the workloads a fleet run watches.
type Scope struct {
	// Namespace to search; "" means all namespaces.
	Namespace string
	// Selector filters by workload labels; "" matches everything in scope.
	Selector string
	// MaxTargets caps the fleet size; 0 means DefaultMaxTargets. A selection
	// past the cap is refused with an OverCeilingError.
	MaxTargets int
	// IncludeJobs adds Jobs to the fan-out. Off by default: batch churn
	// (completed and retrying Jobs) would otherwise drown fleet verdicts.
	IncludeJobs bool
}

// TargetResult pairs one fleet target with its outcome.
type TargetResult struct {
	Target Target
	Result Result
}

// Discover lists the workloads the scope selects, sorted by namespace, kind,
// name. Filtering happens server-side, and each list is capped at the
// ceiling so an over-broad selection is refused without fetching the objects
// beyond it.
func Discover(ctx context.Context, cs kubernetes.Interface, scope Scope) ([]Target, error) {
	if _, err := labels.Parse(scope.Selector); err != nil {
		return nil, fmt.Errorf("invalid selector %q: %w", scope.Selector, err)
	}
	ceiling := scope.MaxTargets
	if ceiling <= 0 {
		ceiling = DefaultMaxTargets
	}
	ns := scope.Namespace
	opts := metav1.ListOptions{LabelSelector: scope.Selector, Limit: int64(ceiling) + 1}

	var targets []Target
	more := false

	deps, err := cs.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, listError(err, "deployments", ns)
	}
	for i := range deps.Items {
		targets = append(targets, Target{Kind: KindDeployment, Namespace: deps.Items[i].Namespace, Name: deps.Items[i].Name})
	}
	more = more || deps.Continue != ""

	stss, err := cs.AppsV1().StatefulSets(ns).List(ctx, opts)
	if err != nil {
		return nil, listError(err, "statefulsets", ns)
	}
	for i := range stss.Items {
		targets = append(targets, Target{Kind: KindStatefulSet, Namespace: stss.Items[i].Namespace, Name: stss.Items[i].Name})
	}
	more = more || stss.Continue != ""

	dss, err := cs.AppsV1().DaemonSets(ns).List(ctx, opts)
	if err != nil {
		return nil, listError(err, "daemonsets", ns)
	}
	for i := range dss.Items {
		targets = append(targets, Target{Kind: KindDaemonSet, Namespace: dss.Items[i].Namespace, Name: dss.Items[i].Name})
	}
	more = more || dss.Continue != ""

	if scope.IncludeJobs {
		jobs, err := cs.BatchV1().Jobs(ns).List(ctx, opts)
		if err != nil {
			return nil, listError(err, "jobs", ns)
		}
		for i := range jobs.Items {
			// Jobs owned by a CronJob still count: each run either completed
			// or has a diagnosable problem.
			targets = append(targets, Target{Kind: KindJob, Namespace: jobs.Items[i].Namespace, Name: jobs.Items[i].Name})
		}
		more = more || jobs.Continue != ""
	}

	if more || len(targets) > ceiling {
		return nil, &OverCeilingError{Matched: len(targets), Ceiling: ceiling}
	}
	if len(targets) == 0 {
		return nil, &NoMatchError{Selector: scope.Selector, Namespace: scope.Namespace}
	}
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return targets, nil
}

func listError(err error, resource, namespace string) error {
	if apierrors.IsForbidden(err) {
		return &PermissionError{Verb: "list", Resource: resource, Namespace: namespace}
	}
	return err
}

// RunFleet watches every workload the scope selects until each settles,
// fails terminally, or opts.Timeout elapses. The whole fleet shares one
// informer set per scope — a fleet of hundreds costs a handful of watches,
// not hundreds — and results keep discovery order (namespace, kind, name) so
// output is deterministic.
func RunFleet(parent context.Context, cs kubernetes.Interface, scope Scope, opts Options) ([]TargetResult, error) {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	start := time.Now()

	pctx, pcancel := context.WithTimeout(parent, opts.Timeout)
	stopPreflight := perf.Phase("preflight")
	targets, err := Discover(pctx, cs, scope)
	if err == nil {
		err = fleetPreflight(pctx, cs, scope.Namespace, targets)
	}
	stopPreflight()
	pcancel()
	if err != nil {
		return nil, err
	}

	// Workload informers are filtered server-side by the scope's selector.
	// Pods, ReplicaSets and ControllerRevisions cannot be — they carry the
	// pod template's labels, not the workload's — so they watch the whole
	// scope unfiltered.
	wf := informers.NewSharedInformerFactoryWithOptions(cs, 0, factoryOptions(scope.Namespace, scope.Selector)...)
	rf := informers.NewSharedInformerFactoryWithOptions(cs, 0, factoryOptions(scope.Namespace, "")...)
	ls := newFleetListers(wf, rf, targets)
	// Shutdown blocks until the informer goroutines exit, and they exit on
	// context cancel — so cancel (registered later, LIFO) must run first.
	defer wf.Shutdown()
	defer rf.Shutdown()
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	stopSync := perf.Phase("sync")
	wf.Start(ctx.Done())
	rf.Start(ctx.Done())
	for _, f := range []informers.SharedInformerFactory{wf, rf} {
		for _, ok := range f.WaitForCacheSync(ctx.Done()) {
			if !ok {
				stopSync()
				return nil, fmt.Errorf("timed out syncing informer caches")
			}
		}
	}
	stopSync()
	defer perf.Phase("watch")()

	watches := make([]*fleetWatch, len(targets))
	for i, tgt := range targets {
		watches[i] = &fleetWatch{target: tgt, src: ls.sourceFor(tgt), tr: newTracker(opts)}
	}

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		now := time.Now()
		open := 0
		for _, w := range watches {
			if w.done {
				continue
			}
			w.observe(now, start)
			if !w.done {
				open++
			}
		}
		if open == 0 {
			return fleetResults(watches), nil
		}
		if opts.Progress != nil {
			opts.Progress(time.Since(start), fmt.Sprintf("%d/%d targets still open", open, len(watches)))
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil, parent.Err()
			}
			for _, w := range watches {
				if w.done {
					continue
				}
				out, reason := w.tr.timeoutVerdict()
				w.finish(out, reason, time.Since(start))
			}
			return fleetResults(watches), nil
		case <-ticker.C:
		}
	}
}

// fleetWatch is one target's state within a fleet run: its source over the
// shared caches and its own tracker.
type fleetWatch struct {
	target Target
	src    *source
	tr     *tracker
	done   bool
	res    Result
}

func (w *fleetWatch) observe(now, start time.Time) {
	snap, err := w.src.snapshot()
	if err != nil {
		// A target deleted mid-watch is a real observation, not an engine
		// error: report it failed and keep watching the rest of the fleet.
		reason := err.Error()
		var nf *NotFoundError
		if errors.As(err, &nf) {
			reason = fmt.Sprintf("%s was deleted during the watch", w.target)
		}
		w.finish(OutcomeFailed, reason, time.Since(start))
		return
	}
	if out, done := w.tr.observe(now, snap); done {
		w.finish(out, w.tr.lastReason, time.Since(start))
	}
}

func (w *fleetWatch) finish(out Outcome, reason string, elapsed time.Duration) {
	w.done = true
	w.res = Result{Outcome: out, Reason: reason, Elapsed: elapsed, Final: w.tr.observation()}
}

func fleetResults(watches []*fleetWatch) []TargetResult {
	out := make([]TargetResult, len(watches))
	for i, w := range watches {
		out[i] = TargetResult{Target: w.target, Result: w.res}
	}
	return out
}

// fleetListers registers exactly the informers the fleet's kinds need:
// workload kinds on the selector-filtered factory, everything else on the
// unfiltered one. Registration must happen before the factories start.
type fleetListers struct {
	deployments         appslisters.DeploymentLister
	replicaSets         appslisters.ReplicaSetLister
	statefulSets        appslisters.StatefulSetLister
	daemonSets          appslisters.DaemonSetLister
	controllerRevisions appslisters.ControllerRevisionLister
	jobs                batchlisters.JobLister
	pods                corelisters.PodLister
}

func newFleetListers(wf, rf informers.SharedInformerFactory, targets []Target) *fleetListers {
	ls := &fleetListers{pods: rf.Core().V1().Pods().Lister()}
	for _, t := range targets {
		switch t.Kind {
		case KindDeployment:
			ls.deployments = wf.Apps().V1().Deployments().Lister()
			ls.replicaSets = rf.Apps().V1().ReplicaSets().Lister()
		case KindStatefulSet:
			ls.statefulSets = wf.Apps().V1().StatefulSets().Lister()
		case KindDaemonSet:
			ls.daemonSets = wf.Apps().V1().DaemonSets().Lister()
			ls.controllerRevisions = rf.Apps().V1().ControllerRevisions().Lister()
		case KindJob:
			ls.jobs = wf.Batch().V1().Jobs().Lister()
		}
	}
	return ls
}

func (ls *fleetListers) sourceFor(t Target) *source {
	return &source{
		target:              t,
		deployments:         ls.deployments,
		replicaSets:         ls.replicaSets,
		statefulSets:        ls.statefulSets,
		daemonSets:          ls.daemonSets,
		controllerRevisions: ls.controllerRevisions,
		jobs:                ls.jobs,
		pods:                ls.pods,
	}
}

func factoryOptions(namespace, selector string) []informers.SharedInformerOption {
	var fo []informers.SharedInformerOption
	if namespace != "" {
		fo = append(fo, informers.WithNamespace(namespace))
	}
	if selector != "" {
		fo = append(fo, informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }))
	}
	return fo
}

// fleetPreflight probes the list permissions the shared informers depend on,
// like the single-target preflight: informers swallow list errors, so a
// denied list would otherwise surface as a cache-sync hang lasting the full
// timeout.
func fleetPreflight(ctx context.Context, cs kubernetes.Interface, namespace string, targets []Target) error {
	need := map[string]bool{"pods": true}
	for _, t := range targets {
		switch t.Kind {
		case KindDeployment:
			need["replicasets"] = true
		case KindDaemonSet:
			need["controllerrevisions"] = true
		}
	}
	for _, r := range []string{"pods", "replicasets", "controllerrevisions"} {
		if !need[r] {
			continue
		}
		if err := preflightList(ctx, cs, namespace, r); err != nil {
			return err
		}
	}
	return nil
}
