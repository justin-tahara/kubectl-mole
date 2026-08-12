package settle

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// kstatusResult is the part of a kstatus computation the tracker needs.
type kstatusResult struct {
	Status  status.Status
	Message string
}

// snapshot is one observation of the target and its pods, taken from the
// informer caches. Evaluation over it is pure.
type snapshot struct {
	found              bool
	generation         int64
	observedGeneration int64
	kstatus            kstatusResult
	// currentPods are the existing pods of the current revision, sorted by
	// name for deterministic output.
	currentPods []*corev1.Pod
	// oldPods are existing pods of previous revisions (including pods still
	// terminating, which controllers stop counting long before they are
	// gone), sorted by name.
	oldPods []*corev1.Pod
	// completionTerminal marks targets whose success is completion, not held
	// readiness (Jobs, CronJobs, Succeeded pods): a Current kstatus settles
	// immediately — completion cannot regress, so there is no stability
	// window — and pod-level churn (retry pods in phase Failed, restarts
	// under restartPolicy OnFailure) is normal progress, not failure.
	completionTerminal bool
	// terminalFailure, when non-empty, ends the watch failed with this
	// reason regardless of anything else (a suspended Job will never run).
	terminalFailure string
}

// source reads the informer caches needed for one target kind.
type source struct {
	target              Target
	deployments         appslisters.DeploymentLister
	replicaSets         appslisters.ReplicaSetLister
	statefulSets        appslisters.StatefulSetLister
	daemonSets          appslisters.DaemonSetLister
	controllerRevisions appslisters.ControllerRevisionLister
	jobs                batchlisters.JobLister
	cronJobs            batchlisters.CronJobLister
	pods                corelisters.PodLister
}

// newSource registers exactly the informers target's kind needs. Registration
// must happen before the factory starts.
func newSource(factory informers.SharedInformerFactory, target Target) (*source, error) {
	s := &source{target: target, pods: factory.Core().V1().Pods().Lister()}
	switch target.Kind {
	case KindDeployment:
		s.deployments = factory.Apps().V1().Deployments().Lister()
		s.replicaSets = factory.Apps().V1().ReplicaSets().Lister()
	case KindStatefulSet:
		s.statefulSets = factory.Apps().V1().StatefulSets().Lister()
	case KindDaemonSet:
		s.daemonSets = factory.Apps().V1().DaemonSets().Lister()
		s.controllerRevisions = factory.Apps().V1().ControllerRevisions().Lister()
	case KindJob:
		s.jobs = factory.Batch().V1().Jobs().Lister()
	case KindCronJob:
		s.cronJobs = factory.Batch().V1().CronJobs().Lister()
		s.jobs = factory.Batch().V1().Jobs().Lister()
	case KindPod:
		// The pods informer is already registered.
	default:
		return nil, fmt.Errorf("unsupported kind %q", target.Kind)
	}
	return s, nil
}

func (s *source) snapshot() (snapshot, error) {
	switch s.target.Kind {
	case KindDeployment:
		return s.deploymentSnapshot()
	case KindStatefulSet:
		return s.statefulSetSnapshot()
	case KindDaemonSet:
		return s.daemonSetSnapshot()
	case KindJob:
		return s.jobSnapshot()
	case KindCronJob:
		return s.cronJobSnapshot()
	case KindPod:
		return s.podSnapshot()
	}
	return snapshot{}, fmt.Errorf("unsupported kind %q", s.target.Kind)
}

func (s *source) deploymentSnapshot() (snapshot, error) {
	d, err := s.deployments.Deployments(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	ks, err := computeKstatus(d, "apps/v1", "Deployment")
	if err != nil {
		return snapshot{}, err
	}
	pods, err := s.selectorPods(d.Spec.Selector)
	if err != nil {
		return snapshot{}, err
	}
	current, old := splitDeploymentPods(d, s.replicaSets, pods)
	return snapshot{
		found:              true,
		generation:         d.Generation,
		observedGeneration: d.Status.ObservedGeneration,
		kstatus:            ks,
		currentPods:        current,
		oldPods:            old,
	}, nil
}

func (s *source) statefulSetSnapshot() (snapshot, error) {
	sts, err := s.statefulSets.StatefulSets(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	ks, err := computeKstatus(sts, "apps/v1", "StatefulSet")
	if err != nil {
		return snapshot{}, err
	}
	pods, err := s.selectorPods(sts.Spec.Selector)
	if err != nil {
		return snapshot{}, err
	}
	current, old := splitStatefulSetPods(sts, pods)
	return snapshot{
		found:              true,
		generation:         sts.Generation,
		observedGeneration: sts.Status.ObservedGeneration,
		kstatus:            ks,
		currentPods:        current,
		oldPods:            old,
	}, nil
}

func (s *source) daemonSetSnapshot() (snapshot, error) {
	ds, err := s.daemonSets.DaemonSets(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	ks, err := computeKstatus(ds, "apps/v1", "DaemonSet")
	if err != nil {
		return snapshot{}, err
	}
	pods, err := s.selectorPods(ds.Spec.Selector)
	if err != nil {
		return snapshot{}, err
	}
	current, old := splitDaemonSetPods(ds, s.controllerRevisions, pods)
	return snapshot{
		found:              true,
		generation:         ds.Generation,
		observedGeneration: ds.Status.ObservedGeneration,
		kstatus:            ks,
		currentPods:        current,
		oldPods:            old,
	}, nil
}

func (s *source) selectorPods(sel *metav1.LabelSelector) ([]*corev1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, fmt.Errorf("workload selector: %w", err)
	}
	pods, err := s.pods.Pods(s.target.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

func notFoundOr(err error, target Target) error {
	if apierrors.IsNotFound(err) {
		return &NotFoundError{Target: target}
	}
	return err
}

// computeKstatus runs the kstatus readiness computation over the typed
// object. Lister objects lack TypeMeta, so the GVK is set explicitly.
func computeKstatus(obj runtime.Object, apiVersion, kind string) (kstatusResult, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return kstatusResult{}, fmt.Errorf("convert %s for kstatus: %w", kind, err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	res, err := status.Compute(u)
	if err != nil {
		return kstatusResult{}, fmt.Errorf("kstatus compute: %w", err)
	}
	return kstatusResult{Status: res.Status, Message: res.Message}, nil
}
