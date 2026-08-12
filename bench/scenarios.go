package main

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	admv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// scenario is one benchmark case: a reproducible failure (or the healthy
// control), the ground-truth cause, and the command sequences each tool runs
// to find it.
type scenario struct {
	name string
	// full marks the large fan-out scale points, run only with --full.
	full bool
	// truth are case-insensitive regexes over a tool's combined output; the
	// ground-truth cause counts as found only when every one matches.
	// Templated with fixture variables. Empty truth marks a control scenario.
	truth []string
	// pertinent are extra density-matcher regexes beyond the auto-added
	// workload and pod names.
	pertinent []string
	// fleetGeneric drops workload names from the density matcher: when the
	// whole fleet shares one name, it identifies nothing.
	fleetGeneric bool
	setup        func(f *fixture) error
	// await blocks until the failure is externally visible before any tool
	// runs (e.g. the node marked NotReady); most scenarios rely on mole's own
	// watch instead.
	await func(f *fixture) error
	// podNS/podApp locate the failing pod, resolved into $POD after the mole
	// run; empty for scenarios that never create pods.
	podNS, podApp string

	moleArgs    []string
	moleTimeout time.Duration
	naive       [][]string
	expert      [][]string
	kstatus     [][]string
}

func corpus() []scenario {
	return []scenario{
		sigImagePull(), sigCrashLoop(), sigOOMKilled(), sigUnschedulable(),
		sigProbeFailing(), sigPVCPending(), sigQuota(), sigAdmission(),
		collapseNodeNotReady(), controlHealthy(),
		fanout("fanout-50", 50, false),
		fanout("fanout-500", 500, true),
		fanout("fanout-5000", 5000, true),
	}
}

// naiveSeq is the flailing loop the tool replaces: dump everything, describe
// everything, read events, read logs.
func naiveSeq(nsKey string, withLogs bool) [][]string {
	seq := [][]string{
		{"get", "all", "-n", nsKey, "-o", "yaml"},
		{"describe", "pods", "-n", nsKey},
		{"get", "events", "-n", nsKey},
	}
	if withLogs {
		seq = append(seq, []string{"logs", "$POD", "-n", nsKey, "--tail", "-1"})
	}
	return seq
}

func statusSeq(nsKey string) [][]string {
	return [][]string{{"deployments", "-n", nsKey}}
}

func sigImagePull() scenario {
	return scenario{
		name:  "sig-imagepullbackoff",
		truth: []string{`no-such-image`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			return f.deployment(ns, "pull", func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers[0].Image = "ghcr.io/justin-tahara/no-such-image:v1"
				d.Spec.Template.Spec.Containers[0].Command = nil
			})
		},
		podNS: "$NS", podApp: "pull",
		moleArgs: []string{"deployment/pull", "-n", "$NS"}, moleTimeout: 30 * time.Second,
		naive: naiveSeq("$NS", true),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"describe", "pod", "$POD", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func sigCrashLoop() scenario {
	return scenario{
		name:  "sig-crashloopbackoff",
		truth: []string{`exit\s*code:?\s*7`, `crashloop|crash-looping`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			return f.deployment(ns, "crash", func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "echo MOLE-BENCH-MARKER; sleep 1; exit 7"}
			})
		},
		podNS: "$NS", podApp: "crash",
		moleArgs: []string{"deployment/crash", "-n", "$NS"}, moleTimeout: 40 * time.Second,
		naive: naiveSeq("$NS", true),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"describe", "pod", "$POD", "-n", "$NS"},
			{"logs", "$POD", "-n", "$NS", "--previous", "--tail", "20"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func sigOOMKilled() scenario {
	return scenario{
		name:  "sig-oomkilled",
		truth: []string{`oomkilled`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			return f.deployment(ns, "oom", func(d *appsv1.Deployment) {
				c := &d.Spec.Template.Spec.Containers[0]
				c.Command = []string{"sh", "-c", "sleep 2; tail /dev/zero"}
				c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}
				c.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}
			})
		},
		podNS: "$NS", podApp: "oom",
		moleArgs: []string{"deployment/oom", "-n", "$NS"}, moleTimeout: 45 * time.Second,
		naive: naiveSeq("$NS", true),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"describe", "pod", "$POD", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func sigUnschedulable() scenario {
	return scenario{
		name:  "sig-podunschedulable",
		truth: []string{`insufficient cpu`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			return f.deployment(ns, "big", func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("64"),
				}
			})
		},
		podNS: "$NS", podApp: "big",
		moleArgs: []string{"deployment/big", "-n", "$NS"}, moleTimeout: 20 * time.Second,
		naive: naiveSeq("$NS", false),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"describe", "pod", "$POD", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func sigProbeFailing() scenario {
	return scenario{
		name:  "sig-probefailing",
		truth: []string{`readiness probe`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			return f.deployment(ns, "probe", func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
					ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "test -f /never"}}},
					InitialDelaySeconds: 1,
					PeriodSeconds:       2,
					FailureThreshold:    1,
				}
			})
		},
		podNS: "$NS", podApp: "probe",
		moleArgs: []string{"deployment/probe", "-n", "$NS"}, moleTimeout: 30 * time.Second,
		naive: naiveSeq("$NS", true),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"describe", "pod", "$POD", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func sigPVCPending() scenario {
	return scenario{
		name:      "sig-pvcpending",
		truth:     []string{`no-such-class`},
		pertinent: []string{`\bdata\b`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			_, err = f.cs.CoreV1().PersistentVolumeClaims(ns).Create(f.ctx, &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: ptr.To("no-such-class"),
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("create pvc: %w", err)
			}
			return f.deployment(ns, "vol", func(d *appsv1.Deployment) {
				d.Spec.Template.Spec.Volumes = []corev1.Volume{{
					Name:         "data",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
				}}
			})
		},
		podNS: "$NS", podApp: "vol",
		moleArgs: []string{"deployment/vol", "-n", "$NS"}, moleTimeout: 25 * time.Second,
		naive: naiveSeq("$NS", false),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"describe", "pod", "$POD", "-n", "$NS"},
			{"get", "pvc", "-n", "$NS"},
			{"describe", "pvc", "data", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func sigQuota() scenario {
	return scenario{
		name:      "sig-quotaexceeded",
		truth:     []string{`quota`, `no-pods`},
		pertinent: []string{`no-pods`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			_, err = f.cs.CoreV1().ResourceQuotas(ns).Create(f.ctx, &corev1.ResourceQuota{
				ObjectMeta: metav1.ObjectMeta{Name: "no-pods"},
				Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("0")}},
			}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("create quota: %w", err)
			}
			return f.deployment(ns, "quota")
		},
		moleArgs: []string{"deployment/quota", "-n", "$NS"}, moleTimeout: 20 * time.Second,
		naive: naiveSeq("$NS", false),
		// No pods exist, so the expert reads the ReplicaSet: describe deploy
		// truncates condition messages, describe rs carries the full
		// FailedCreate event.
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"get", "deploy", "-n", "$NS"},
			{"describe", "rs", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

// sigAdmission uses a ValidatingAdmissionPolicy — the server-free admission
// path — scoped to the scenario namespace by label. The canary loop waits
// for the policy to become active before the workload is created.
func sigAdmission() scenario {
	return scenario{
		name:      "sig-admissionrejected",
		truth:     []string{`denied`, `bench-deny`},
		pertinent: []string{`bench-deny`},
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", map[string]string{"mole-bench-vap": f.run})
			if err != nil {
				return err
			}
			selector := &metav1.LabelSelector{MatchLabels: map[string]string{"mole-bench-vap": f.run}}
			policyName := "bench-deny-" + f.run

			policy := &admv1.ValidatingAdmissionPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: policyName},
				Spec: admv1.ValidatingAdmissionPolicySpec{
					FailurePolicy: ptr.To(admv1.Fail),
					MatchConstraints: &admv1.MatchResources{
						NamespaceSelector: selector,
						ResourceRules: []admv1.NamedRuleWithOperations{{
							RuleWithOperations: admv1.RuleWithOperations{
								Operations: []admv1.OperationType{admv1.Create},
								Rule:       admv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
							},
						}},
					},
					Validations: []admv1.Validation{{Expression: "false", Message: "pods are frozen in this namespace"}},
				},
			}
			if _, err := f.cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(f.ctx, policy, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create policy: %w", err)
			}
			f.cleanups = append(f.cleanups, func() {
				_ = f.cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(context.Background(), policyName, metav1.DeleteOptions{})
			})
			binding := &admv1.ValidatingAdmissionPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{Name: policyName},
				Spec: admv1.ValidatingAdmissionPolicyBindingSpec{
					PolicyName:        policyName,
					ValidationActions: []admv1.ValidationAction{admv1.Deny},
					MatchResources:    &admv1.MatchResources{NamespaceSelector: selector},
				},
			}
			if _, err := f.cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(f.ctx, binding, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create binding: %w", err)
			}
			f.cleanups = append(f.cleanups, func() {
				_ = f.cs.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(context.Background(), policyName, metav1.DeleteOptions{})
			})

			// Canary: create bare pods until the policy denies them, so the
			// workload's ReplicaSet is guaranteed to hit an active policy.
			deadline := time.Now().Add(30 * time.Second)
			for {
				canary := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{GenerateName: "canary-"},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "c", Image: benchImage, Command: []string{"sh", "-c", "sleep 1"},
					}}},
				}
				created, err := f.cs.CoreV1().Pods(ns).Create(f.ctx, canary, metav1.CreateOptions{})
				if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
					break
				}
				if err == nil {
					_ = f.cs.CoreV1().Pods(ns).Delete(f.ctx, created.Name, metav1.DeleteOptions{})
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("admission policy never became active")
				}
				time.Sleep(time.Second)
			}
			return f.deployment(ns, "app")
		},
		moleArgs: []string{"deployment/app", "-n", "$NS"}, moleTimeout: 20 * time.Second,
		naive: naiveSeq("$NS", false),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"get", "deploy", "-n", "$NS"},
			{"describe", "rs", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

// collapseNodeNotReady is the causal-collapse case from the design: a node
// goes dark under two workloads; the correct answer names the node once, not
// four pod symptoms.
func collapseNodeNotReady() scenario {
	return scenario{
		name:  "collapse-nodenotready",
		truth: []string{`$NODE`, `not\s*ready`},
		setup: func(f *fixture) error {
			victim, err := f.workerNode()
			if err != nil {
				return err
			}
			f.vars["$NODE"] = victim
			f.notePertinent(victim)
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			pin := func(d *appsv1.Deployment) {
				d.Spec.Replicas = ptr.To(int32(2))
				d.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": victim}
			}
			if err := f.deployment(ns, "svc-a", pin); err != nil {
				return err
			}
			if err := f.deployment(ns, "svc-b", pin); err != nil {
				return err
			}
			if err := f.waitPodsReady(ns, "svc-a", 2, 90*time.Second); err != nil {
				return err
			}
			if err := f.waitPodsReady(ns, "svc-b", 2, 90*time.Second); err != nil {
				return err
			}
			return f.pauseNode(victim)
		},
		await: func(f *fixture) error {
			return f.awaitNodeNotReady(f.vars["$NODE"], 150*time.Second)
		},
		podNS: "$NS", podApp: "svc-a",
		moleArgs: []string{"-n", "$NS"}, moleTimeout: 45 * time.Second,
		naive: naiveSeq("$NS", true),
		expert: [][]string{
			{"get", "pods", "-n", "$NS", "-o", "wide"},
			{"get", "nodes"},
			{"describe", "node", "$NODE"},
		},
		kstatus: statusSeq("$NS"),
	}
}

func controlHealthy() scenario {
	return scenario{
		name: "control-healthy",
		setup: func(f *fixture) error {
			ns, err := f.namespace("$NS", nil)
			if err != nil {
				return err
			}
			return f.deployment(ns, "web", func(d *appsv1.Deployment) {
				d.Spec.Replicas = ptr.To(int32(2))
			})
		},
		moleArgs: []string{"deployment/web", "-n", "$NS"}, moleTimeout: 60 * time.Second,
		naive: naiveSeq("$NS", false),
		expert: [][]string{
			{"get", "pods", "-n", "$NS"},
			{"get", "deploy", "-n", "$NS"},
		},
		kstatus: statusSeq("$NS"),
	}
}

// fanout builds a fleet of n namespaces sharing one workload label: n-3
// settled (zero-replica) fleets and 3 identical crashers, so the whole run
// must collapse to one cause. n == the fan-out scale point from the design.
func fanout(name string, n int, full bool) scenario {
	timeout := 45 * time.Second
	if n >= 500 {
		timeout = 90 * time.Second
	}
	if n >= 5000 {
		timeout = 150 * time.Second
	}
	return scenario{
		name:  name,
		full:  full,
		truth: []string{`crashloop|crash-looping`},
		// The whole fleet shares the workload name "app" — realistic for
		// stamped-out tenants, but useless for telling failing from healthy —
		// so density falls back to pod-level terms.
		fleetGeneric: true,
		setup: func(f *fixture) error {
			f.vars["$FLEETSEL"] = "bench-fleet=" + f.run
			label := func(d *appsv1.Deployment) {
				d.Labels["bench-fleet"] = f.run
			}
			// Crashers first: the controller works its queue in order, and
			// the failure must exist long before the quiet tail is done.
			for i := 0; i < 3; i++ {
				key := ""
				if i == 0 {
					key = "$NSFAIL"
				}
				ns, err := f.namespace(key, nil)
				if err != nil {
					return err
				}
				if err := f.deployment(ns, "app", label, func(d *appsv1.Deployment) {
					d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 1; exit 7"}
				}); err != nil {
					return err
				}
			}
			for i := 0; i < n-3; i++ {
				ns, err := f.namespace("", nil)
				if err != nil {
					return err
				}
				if err := f.deployment(ns, "app", label, func(d *appsv1.Deployment) {
					d.Spec.Replicas = ptr.To(int32(0))
				}); err != nil {
					return err
				}
			}
			return nil
		},
		// The scenario is a converged fleet with three crashers, not a race
		// against the controller's own queue: at 5000 deployments the
		// controller (default 20 QPS) needs minutes to create ReplicaSets.
		// Every tool measures against the converged state.
		await: func(f *fixture) error {
			if err := f.awaitFleetObserved("bench-fleet="+f.run, 10*time.Minute); err != nil {
				return err
			}
			_, err := f.failingPod("$NSFAIL", "app")
			return err
		},
		podNS: "$NSFAIL", podApp: "app",
		moleArgs: []string{"-A", "-l", "$FLEETSEL"}, moleTimeout: timeout,
		naive: [][]string{
			{"get", "all", "-A", "-o", "yaml"},
			{"get", "events", "-A"},
			{"describe", "pod", "$POD", "-n", "$NSFAIL"},
		},
		expert: [][]string{
			{"get", "deploy", "-A", "-l", "$FLEETSEL"},
			{"get", "pods", "-A"},
			{"describe", "pod", "$POD", "-n", "$NSFAIL"},
			{"logs", "$POD", "-n", "$NSFAIL", "--previous", "--tail", "20"},
		},
		kstatus: [][]string{{"deployments", "-A", "-l", "$FLEETSEL"}},
	}
}
