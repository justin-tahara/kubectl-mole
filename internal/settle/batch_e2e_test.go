//go:build e2e

// End-to-end settle semantics for Jobs, CronJobs, and bare Pods: success is
// completion, retries are progress, suspension is terminal.
package settle_test

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

func newJob(name string, mut ...func(*batchv1.Job)) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "main",
						Image:   image,
						Command: []string{"sh", "-c", "true"},
					}},
				},
			},
		},
	}
	for _, m := range mut {
		m(j)
	}
	return j
}

func createJob(t *testing.T, cs *kubernetes.Clientset, ns string, j *batchv1.Job) {
	t.Helper()
	if _, err := cs.BatchV1().Jobs(ns).Create(context.Background(), j, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create job: %v", err)
	}
}

func runKind(t *testing.T, cs *kubernetes.Clientset, kind settle.Kind, ns, name string, timeout time.Duration) settle.Result {
	t.Helper()
	res, err := settle.Run(context.Background(), cs,
		settle.Target{Kind: kind, Namespace: ns, Name: name},
		settle.Options{Timeout: timeout, StableFor: 5 * time.Second})
	if err != nil {
		t.Fatalf("settle.Run: %v", err)
	}
	t.Logf("outcome=%s reason=%q", res.Outcome, res.Reason)
	return res
}

func TestJobCompletesSettles(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	createJob(t, cs, ns, newJob("ok"))

	res := runKind(t, cs, settle.KindJob, ns, "ok", 60*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("a completing job must settle, got %s (%s)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "completed") {
		t.Fatalf("reason should say completed, got %q", res.Reason)
	}
}

func TestJobBackoffLimitFails(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	createJob(t, cs, ns, newJob("boom", func(j *batchv1.Job) {
		j.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "exit 3"}
	}))

	// Two failed pods (backoffLimit 1) end the job; the second retry waits
	// out an exponential backoff, so give it room.
	res := runKind(t, cs, settle.KindJob, ns, "boom", 3*time.Minute)
	if res.Outcome != settle.OutcomeFailed {
		t.Fatalf("backoffLimit exhaustion must fail, got %s (%s)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "BackoffLimitExceeded") && !strings.Contains(strings.ToLower(res.Reason), "backoff") {
		t.Fatalf("reason should carry the job failure condition, got %q", res.Reason)
	}

	// The retry pods died under restartPolicy Never: diagnosis must name
	// the exit code through ContainerFailed.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rep := signatures.Diagnose(ctx, cs, signatures.TargetRef{Kind: "Job", Namespace: ns, Name: "boom"},
		res.Final.CurrentPods, res.Final.OldPods)
	for _, f := range rep.Findings {
		if f.Signature == "ContainerFailed" && strings.Contains(f.Cause, "exited with code 3") {
			t.Logf("finding: %s: %s (chain %v)", f.Signature, f.Cause, f.Chain)
			return
		}
	}
	t.Fatalf("no ContainerFailed finding with exit code 3; findings: %+v", rep.Findings)
}

func TestSuspendedJobFails(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	createJob(t, cs, ns, newJob("paused", func(j *batchv1.Job) {
		j.Spec.Suspend = ptr.To(true)
	}))

	res := runKind(t, cs, settle.KindJob, ns, "paused", 30*time.Second)
	if res.Outcome != settle.OutcomeFailed || !strings.Contains(res.Reason, "suspended") {
		t.Fatalf("a suspended job is terminal, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Elapsed > 15*time.Second {
		t.Fatalf("suspension must be detected immediately, took %s", res.Elapsed)
	}
}

func TestCronJobNeverScheduledIsProgressing(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "yearly"},
		Spec: batchv1.CronJobSpec{
			// Fires once a year: never during this watch.
			Schedule:    "0 0 1 1 *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: newJob("t").Spec},
		},
	}
	if _, err := cs.BatchV1().CronJobs(ns).Create(context.Background(), cj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create cronjob: %v", err)
	}

	res := runKind(t, cs, settle.KindCronJob, ns, "yearly", 15*time.Second)
	if res.Outcome != settle.OutcomeProgressing {
		t.Fatalf("nothing scheduled yet is progress, got %s (%s)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "no job scheduled yet") {
		t.Fatalf("reason should say no job scheduled, got %q", res.Reason)
	}
}

func TestCronJobSettlesOnScheduledJob(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "minutely"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "* * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: newJob("t").Spec},
		},
	}
	if _, err := cs.BatchV1().CronJobs(ns).Create(context.Background(), cj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create cronjob: %v", err)
	}

	// Up to 60s until the schedule fires, a few seconds for the run.
	res := runKind(t, cs, settle.KindCronJob, ns, "minutely", 2*time.Minute)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("the scheduled job completed, want settled, got %s (%s)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "job minutely-") {
		t.Fatalf("reason should name the scheduled job, got %q", res.Reason)
	}
}

func TestPodSettlesAndCrashFails(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)

	mk := func(name string, cmd []string) {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: ptr.To(int64(2)),
				Containers:                    []corev1.Container{{Name: "main", Image: image, Command: cmd}},
			},
		}
		if _, err := cs.CoreV1().Pods(ns).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod %s: %v", name, err)
		}
	}
	mk("steady", []string{"sh", "-c", "sleep 3600"})
	mk("crash", []string{"sh", "-c", "sleep 1; exit 7"})

	res := runKind(t, cs, settle.KindPod, ns, "steady", 60*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("a ready pod must settle, got %s (%s)", res.Outcome, res.Reason)
	}

	res = runKind(t, cs, settle.KindPod, ns, "crash", 45*time.Second)
	if res.Outcome != settle.OutcomeFailed {
		t.Fatalf("a crash-looping pod must fail, got %s (%s)", res.Outcome, res.Reason)
	}
}

// TestPodSucceededSettles: a run-once pod that finished is done, not
// flapping — Succeeded is terminally settled.
func TestPodSucceededSettles(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "once"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "main", Image: image, Command: []string{"sh", "-c", "true"}}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	res := runKind(t, cs, settle.KindPod, ns, "once", 60*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("a Succeeded pod must settle, got %s (%s)", res.Outcome, res.Reason)
	}
}
