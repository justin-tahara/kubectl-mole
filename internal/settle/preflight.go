package settle

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// preflight performs the reads the watch depends on, once, directly against
// the API server. Informers swallow list errors — a denied watch would
// otherwise surface as a cache-sync hang that lasts the full timeout. This
// turns an RBAC denial or a missing target into an immediate typed error.
func preflight(ctx context.Context, cs kubernetes.Interface, target Target) error {
	var err error
	switch target.Kind {
	case KindDeployment:
		_, err = cs.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	case KindStatefulSet:
		_, err = cs.AppsV1().StatefulSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	case KindDaemonSet:
		_, err = cs.AppsV1().DaemonSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	case KindJob:
		_, err = cs.BatchV1().Jobs(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	case KindCronJob:
		_, err = cs.BatchV1().CronJobs(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	case KindPod:
		_, err = cs.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &NotFoundError{Target: target}
		}
		if apierrors.IsForbidden(err) {
			return &PermissionError{Verb: "get", Resource: resourceName(target.Kind), Namespace: target.Namespace}
		}
		return err
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
			return err
		}
	}
	return nil
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
