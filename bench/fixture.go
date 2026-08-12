package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// benchImage is the one container image every scenario uses, pinned. Docker
// Hub official image via the ECR Public mirror: no anonymous-pull limits.
const benchImage = "public.ecr.aws/docker/library/busybox:1.36"

// fixture is one scenario's cluster state: its namespaces, template
// variables, and cleanups. Everything it creates carries the run label so a
// crashed bench run is greppable and deletable by hand.
type fixture struct {
	ctx        context.Context
	cs         *kubernetes.Clientset
	run        string
	vars       map[string]string
	namespaces []string
	cleanups   []func()
	// pertinent collects workload names for the signal-density matcher.
	pertinent []string
}

func newFixture(ctx context.Context, cs *kubernetes.Clientset) *fixture {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &fixture{ctx: ctx, cs: cs, run: hex.EncodeToString(b), vars: map[string]string{}}
}

func (f *fixture) teardown() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
	for _, ns := range f.namespaces {
		_ = f.cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	}
}

// namespace creates a run-labeled namespace and, when key is non-empty,
// stores its generated name as a template variable (e.g. "$NS").
func (f *fixture) namespace(key string, extraLabels map[string]string) (string, error) {
	labels := map[string]string{"mole-bench": f.run}
	for k, v := range extraLabels {
		labels[k] = v
	}
	ns, err := f.cs.CoreV1().Namespaces().Create(f.ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "mole-bench-", Labels: labels},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create namespace: %w", err)
	}
	f.namespaces = append(f.namespaces, ns.Name)
	if key != "" {
		f.vars[key] = ns.Name
	}
	return ns.Name, nil
}

func (f *fixture) deployment(ns, name string, mut ...func(*appsv1.Deployment)) error {
	d := benchDeployment(f.run, name, mut...)
	if _, err := f.cs.AppsV1().Deployments(ns).Create(f.ctx, d, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create deployment %s/%s: %w", ns, name, err)
	}
	f.notePertinent(name)
	return nil
}

// benchDeployment builds the standard bench deployment object; pure, so the
// concurrent fleet staging can share it with the serial helper.
func benchDeployment(run, name string, mut ...func(*appsv1.Deployment)) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"mole-bench": run}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(2)),
					Containers: []corev1.Container{{
						Name:    "main",
						Image:   benchImage,
						Command: []string{"sh", "-c", "sleep 3600"},
					}},
				},
			},
		},
	}
	for _, m := range mut {
		m(d)
	}
	return d
}

// fleetQuiet stages n identical fleet members — namespace plus deployment
// "app" each — through a worker pool. Staging is the only concurrent phase
// of the bench: nothing is being measured yet, and the serial loop was
// latency-bound at one blocking round-trip per object (10,000 of them for
// the 5,000-namespace point). Shared fixture state is only touched after
// the pool drains.
func (f *fixture) fleetQuiet(n int, mut ...func(*appsv1.Deployment)) error {
	const workers = 32
	type staged struct {
		ns  string
		err error
	}
	jobs := make(chan struct{})
	results := make(chan staged, n)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				ns, err := f.cs.CoreV1().Namespaces().Create(f.ctx, &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{GenerateName: "mole-bench-", Labels: map[string]string{"mole-bench": f.run}},
				}, metav1.CreateOptions{})
				if err != nil {
					results <- staged{err: fmt.Errorf("create namespace: %w", err)}
					continue
				}
				d := benchDeployment(f.run, "app", mut...)
				if _, err := f.cs.AppsV1().Deployments(ns.Name).Create(f.ctx, d, metav1.CreateOptions{}); err != nil {
					results <- staged{ns: ns.Name, err: fmt.Errorf("create deployment %s/app: %w", ns.Name, err)}
					continue
				}
				results <- staged{ns: ns.Name}
			}
		}()
	}
	go func() {
		for i := 0; i < n; i++ {
			jobs <- struct{}{}
		}
		close(jobs)
	}()
	wg.Wait()
	close(results)

	var firstErr error
	for r := range results {
		if r.ns != "" {
			f.namespaces = append(f.namespaces, r.ns)
		}
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	f.notePertinent("app")
	return firstErr
}

func (f *fixture) notePertinent(term string) {
	for _, p := range f.pertinent {
		if p == term {
			return
		}
	}
	f.pertinent = append(f.pertinent, term)
}

// failingPod resolves "the pod an SRE would look at": the first (by name)
// not-ready pod of the workload. Polls because scenarios reach their failure
// state asynchronously.
func (f *fixture) failingPod(nsKey, app string) (string, error) {
	ns := f.vars[nsKey]
	deadline := time.Now().Add(90 * time.Second)
	for {
		list, err := f.cs.CoreV1().Pods(ns).List(f.ctx, metav1.ListOptions{LabelSelector: "app=" + app})
		if err != nil {
			return "", err
		}
		var names []string
		for i := range list.Items {
			if !benchPodReady(&list.Items[i]) {
				names = append(names, list.Items[i].Name)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			return names[0], nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no failing pod for app=%s in %s", app, ns)
		}
		time.Sleep(2 * time.Second)
	}
}

func (f *fixture) waitPodsReady(ns, app string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		list, err := f.cs.CoreV1().Pods(ns).List(f.ctx, metav1.ListOptions{LabelSelector: "app=" + app})
		if err != nil {
			return err
		}
		ready := 0
		for i := range list.Items {
			if benchPodReady(&list.Items[i]) {
				ready++
			}
		}
		if ready >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d/%d pods of app=%s ready in %s", ready, want, app, ns)
		}
		time.Sleep(2 * time.Second)
	}
}

// awaitFleetObserved waits until the deployment controller has observed
// every fleet deployment at least once — the point where a quiet fleet can
// settle. Large fleets queue behind the controller's own rate limits, and
// the bench measures the converged state, not the controller's backlog.
func (f *fixture) awaitFleetObserved(selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		list, err := f.cs.AppsV1().Deployments("").List(f.ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		pending := 0
		for i := range list.Items {
			if list.Items[i].Status.ObservedGeneration < list.Items[i].Generation {
				pending++
			}
		}
		if pending == 0 && len(list.Items) > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d fleet deployments still unobserved after %s", pending, len(list.Items), timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

// workerNode picks the first (by name) non-control-plane node. The bench
// cluster config provides two workers for the node scenarios.
func (f *fixture) workerNode() (string, error) {
	nodes, err := f.cs.CoreV1().Nodes().List(f.ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var names []string
	for _, n := range nodes.Items {
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; !cp {
			names = append(names, n.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("no worker node; the node scenario needs the multi-node bench cluster (bench/kind.yaml)")
	}
	return names[0], nil
}

// pauseNode freezes a kind node's container, which the control plane reads
// as the node going dark; the cleanup unpauses it. Bench-only: the tool
// itself never touches docker.
func (f *fixture) pauseNode(name string) error {
	if out, err := exec.Command("docker", "pause", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker pause %s: %v: %s", name, err, out)
	}
	f.cleanups = append(f.cleanups, func() {
		_ = exec.Command("docker", "unpause", name).Run()
	})
	return nil
}

func (f *fixture) awaitNodeNotReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n, err := f.cs.CoreV1().Nodes().Get(f.ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s still Ready after %s", name, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// expand substitutes template variables, longest keys first so $NSFAIL is
// never half-eaten by $NS.
func (f *fixture) expand(s string) string {
	keys := make([]string, 0, len(f.vars))
	for k := range f.vars {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, f.vars[k])
	}
	return s
}

func (f *fixture) expandArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = f.expand(a)
	}
	return out
}

func (f *fixture) expandSeq(seq [][]string) [][]string {
	out := make([][]string, len(seq))
	for i, args := range seq {
		out[i] = f.expandArgs(args)
	}
	return out
}

func benchPodReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
