package settle

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// watchScope narrows the related-object informer caches to the target's own
// objects: the workload's spec.selector (which its pods, ReplicaSets and
// ControllerRevisions carry across every revision), or the pod's own name
// for a bare Pod. Empty means unfiltered — a CronJob's owned Jobs carry no
// guaranteed label, and a missing selector falls back to the whole
// namespace rather than a wrong cache.
type watchScope struct {
	labelSelector string
	fieldSelector string
}

// preflight performs the reads the watch depends on, once, directly against
// the API server. Informers swallow list errors — a denied watch would
// otherwise surface as a cache-sync hang that lasts the full timeout. This
// turns an RBAC denial or a missing target into an immediate typed error,
// and the fetched workload supplies the selector that scopes the caches.
func preflight(ctx context.Context, cs kubernetes.Interface, target Target) (watchScope, error) {
	var scope watchScope
	var err error
	sel := func(s *metav1.LabelSelector) string {
		if s == nil {
			return ""
		}
		return metav1.FormatLabelSelector(s)
	}
	switch target.Kind {
	case KindDeployment:
		var d *appsv1.Deployment
		if d, err = cs.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{}); err == nil {
			scope.labelSelector = sel(d.Spec.Selector)
		}
	case KindStatefulSet:
		var s *appsv1.StatefulSet
		if s, err = cs.AppsV1().StatefulSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{}); err == nil {
			scope.labelSelector = sel(s.Spec.Selector)
		}
	case KindDaemonSet:
		var d *appsv1.DaemonSet
		if d, err = cs.AppsV1().DaemonSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{}); err == nil {
			scope.labelSelector = sel(d.Spec.Selector)
		}
	case KindJob:
		var j *batchv1.Job
		if j, err = cs.BatchV1().Jobs(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{}); err == nil {
			scope.labelSelector = sel(j.Spec.Selector)
		}
	case KindCronJob:
		_, err = cs.BatchV1().CronJobs(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	case KindPod:
		if _, err = cs.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{}); err == nil {
			scope.fieldSelector = "metadata.name=" + target.Name
		}
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return watchScope{}, &NotFoundError{Target: target}
		}
		if apierrors.IsForbidden(err) {
			return watchScope{}, &PermissionError{Verb: "get", Resource: resourceName(target.Kind), Namespace: target.Namespace}
		}
		return watchScope{}, err
	}

	resources := []string{"pods"}
	switch target.Kind {
	case KindDeployment:
		resources = append(resources, "replicasets")
	case KindDaemonSet:
		resources = append(resources, "controllerrevisions")
	case KindCronJob:
		resources = append(resources, "jobs")
	}
	for _, r := range resources {
		if err := preflightList(ctx, cs, target.Namespace, r); err != nil {
			return watchScope{}, err
		}
	}
	return scope, nil
}

func preflightList(ctx context.Context, cs kubernetes.Interface, namespace, resource string) error {
	opts := metav1.ListOptions{Limit: 1}
	var err error
	switch resource {
	case "pods":
		_, err = cs.CoreV1().Pods(namespace).List(ctx, opts)
	case "replicasets":
		_, err = cs.AppsV1().ReplicaSets(namespace).List(ctx, opts)
	case "controllerrevisions":
		_, err = cs.AppsV1().ControllerRevisions(namespace).List(ctx, opts)
	case "jobs":
		_, err = cs.BatchV1().Jobs(namespace).List(ctx, opts)
	}
	if apierrors.IsForbidden(err) {
		return &PermissionError{Verb: "list", Resource: resource, Namespace: namespace}
	}
	return err
}

func resourceName(k Kind) string {
	switch k {
	case KindDeployment:
		return "deployments"
	case KindStatefulSet:
		return "statefulsets"
	case KindDaemonSet:
		return "daemonsets"
	case KindJob:
		return "jobs"
	case KindCronJob:
		return "cronjobs"
	case KindPod:
		return "pods"
	}
	return string(k)
}
