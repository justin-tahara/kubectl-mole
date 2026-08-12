package settle

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

// Kind is a workload kind the settle engine understands.
type Kind string

const (
	KindDeployment  Kind = "Deployment"
	KindStatefulSet Kind = "StatefulSet"
	KindDaemonSet   Kind = "DaemonSet"
)

// Target identifies one workload to watch.
type Target struct {
	Kind      Kind
	Namespace string
	Name      string
}

func (t Target) String() string { return fmt.Sprintf("%s/%s", t.Kind, t.Name) }

// Outcome classifies how a watch ended.
type Outcome string

const (
	// OutcomeSettled means the workload was healthy continuously for the
	// stability window.
	OutcomeSettled Outcome = "settled"
	// OutcomeProgressing means the timeout elapsed while the rollout was
	// still legitimately progressing. Distinct from failed on purpose:
	// automation must not roll back on it.
	OutcomeProgressing Outcome = "progressing"
	// OutcomeFailed means terminal failure indicators were present.
	OutcomeFailed Outcome = "failed"
)

// Result is the engine's verdict for one target.
type Result struct {
	Outcome Outcome
	Reason  string
	Elapsed time.Duration
	// Final is the last observation of the watch, for downstream diagnosis.
	Final Observation
}

// Observation is the state of the target at the end of the watch.
type Observation struct {
	// CurrentPods are the existing current-revision pods, sorted by name.
	CurrentPods []*corev1.Pod
	// OldPods are previous-revision pods still present, sorted by name.
	// A rollout is not settled while any exist, and one of them wedged
	// behind a finalizer is a diagnosable cause.
	OldPods []*corev1.Pod
}

// Options tune the watch.
type Options struct {
	// Timeout is the max wall-clock to wait for settle.
	Timeout time.Duration
	// StableFor is how long the target must hold a healthy state
	// continuously before it counts as settled.
	StableFor time.Duration
	// Interval between evaluations of the informer caches. Defaults to 1s.
	Interval time.Duration
}

// Run watches target until it settles, fails terminally, or opts.Timeout
// elapses. It holds informers open for the whole watch and evaluates their
// caches on a fixed interval; it never re-lists against the API server.
func Run(parent context.Context, cs kubernetes.Interface, target Target, opts Options) (Result, error) {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	start := time.Now()

	pctx, pcancel := context.WithTimeout(parent, opts.Timeout)
	err := preflight(pctx, cs, target)
	pcancel()
	if err != nil {
		return Result{}, err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(target.Namespace))
	src, err := newSource(factory, target)
	if err != nil {
		return Result{}, err
	}
	// Shutdown blocks until the informer goroutines exit, and they exit on
	// context cancel — so cancel (registered later, LIFO) must run first, or
	// a settled verdict hangs until the full timeout.
	defer factory.Shutdown()
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	factory.Start(ctx.Done())
	for _, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return Result{}, fmt.Errorf("timed out syncing informer caches for %s", target)
		}
	}

	tr := newTracker(opts)
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		snap, err := src.snapshot()
		if err != nil {
			return Result{}, err
		}
		if out, done := tr.observe(time.Now(), snap); done {
			return Result{Outcome: out, Reason: tr.lastReason, Elapsed: time.Since(start), Final: tr.observation()}, nil
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return Result{}, parent.Err()
			}
			out, reason := tr.timeoutVerdict()
			return Result{Outcome: out, Reason: reason, Elapsed: time.Since(start), Final: tr.observation()}, nil
		case <-ticker.C:
		}
	}
}
