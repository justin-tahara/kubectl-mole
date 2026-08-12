package signatures

// Tests for the everyday-failure detectors (v0.2 M11): config errors, volume
// mount/attach, sandbox creation, start failures, evictions, stuck
// terminations, and init-container attribution.

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestConfigMissing(t *testing.T) {
	p := basePod("a")
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerConfigError", Message: `configmap "app-config" not found`,
		}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "ConfigMissing" {
		t.Fatalf("want ConfigMissing, got %+v", f)
	}
	if !strings.Contains(f.Cause, `configmap "app-config" not found`) {
		t.Fatalf("cause should name the missing object, got %q", f.Cause)
	}
}

func TestStartFailedOutranksCrashLoop(t *testing.T) {
	// The classic shape: exec-not-found start failures accumulate restarts
	// and the pod sits in CrashLoopBackOff, but the actionable fact is that
	// the binary does not exist.
	p := basePod("a")
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:   "StartError",
			ExitCode: 128,
			Message:  `failed to create containerd task: OCI runtime create failed: exec: "/no-such-binary": stat /no-such-binary: no such file or directory: unknown`,
		}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "ContainerStartFailed" {
		t.Fatalf("start failure must outrank the crash loop, got %+v", f)
	}
	if !strings.Contains(f.Cause, "no-such-binary") {
		t.Fatalf("cause should carry the runtime error, got %q", f.Cause)
	}
}

func TestStartFailedIgnoresRecoveredPod(t *testing.T) {
	p := basePod("a")
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "main",
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "StartError"}},
	}}
	if f := diagnosePod(emptyCtx(), p); f != nil {
		t.Fatalf("a ready pod's old start failure is history, got %+v", f)
	}
}

func TestVolumeMountFailed(t *testing.T) {
	p := basePod("a")
	p.Spec.NodeName = "node-1"
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}}
	c := emptyCtx()
	c.PodEvents = func(*corev1.Pod) []corev1.Event {
		return []corev1.Event{{Reason: "FailedMount", Message: `MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`}}
	}
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "VolumeMountFailed" {
		t.Fatalf("want VolumeMountFailed, got %+v", f)
	}
	if !strings.Contains(f.Cause, `secret "db-creds" not found`) {
		t.Fatalf("cause should carry the mount error, got %q", f.Cause)
	}
}

func TestVolumeAttachFailedNamesAttach(t *testing.T) {
	p := basePod("a")
	p.Spec.NodeName = "node-1"
	c := emptyCtx()
	c.PodEvents = func(*corev1.Pod) []corev1.Event {
		return []corev1.Event{{Reason: "FailedAttachVolume", Message: "Multi-Attach error for volume pvc-1: volume is already exclusively attached to one node"}}
	}
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "VolumeMountFailed" || !strings.HasPrefix(f.Cause, "cannot attach") {
		t.Fatalf("attach failures must say attach, got %+v", f)
	}
}

// TestStaleMountEventDoesNotOutrankCrashLoop: a pod that mounted fine after
// early FailedMount retries and now crash-loops must be diagnosed as
// crash-looping — the mount event is history.
func TestStaleMountEventDoesNotOutrankCrashLoop(t *testing.T) {
	p := basePod("a")
	p.Spec.NodeName = "node-1"
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	c := emptyCtx()
	c.PodEvents = func(*corev1.Pod) []corev1.Event {
		return []corev1.Event{{Reason: "FailedMount", Message: "timed out waiting for the condition"}}
	}
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "CrashLoopBackOff" {
		t.Fatalf("stale mount event must not win, got %+v", f)
	}
}

func TestSandboxFailed(t *testing.T) {
	p := basePod("a")
	p.Spec.NodeName = "node-1"
	c := emptyCtx()
	c.PodEvents = func(*corev1.Pod) []corev1.Event {
		return []corev1.Event{{Reason: "FailedCreatePodSandBox", Message: "failed to setup network for sandbox: no IP addresses available in range"}}
	}
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "PodSandboxFailed" {
		t.Fatalf("want PodSandboxFailed, got %+v", f)
	}
	if !strings.Contains(f.Cause, "no IP addresses available") {
		t.Fatalf("cause should carry the CNI error, got %q", f.Cause)
	}
}

func TestEvicted(t *testing.T) {
	p := basePod("a")
	p.Status.Phase = corev1.PodFailed
	p.Status.Reason = "Evicted"
	p.Status.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 5Mi.\ncontainer main was using 52Mi"
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "PodEvicted" {
		t.Fatalf("want PodEvicted, got %+v", f)
	}
	if !strings.Contains(f.Cause, "ephemeral local storage") || strings.Contains(f.Cause, "\n") {
		t.Fatalf("cause should carry the first line of the eviction message, got %q", f.Cause)
	}
	if len(f.Evidence) == 0 || !strings.Contains(f.Evidence[0].Text, "52Mi") {
		t.Fatalf("full eviction message belongs in evidence, got %+v", f.Evidence)
	}
}

func TestStuckTerminating(t *testing.T) {
	p := basePod("a")
	p.Finalizers = []string{"example.com/cleanup"}
	p.DeletionTimestamp = &metav1.Time{Time: testNow.Add(-2 * time.Minute)}
	p.DeletionGracePeriodSeconds = ptr.To(int64(30))
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "PodStuckTerminating" {
		t.Fatalf("want PodStuckTerminating, got %+v", f)
	}
	if !strings.Contains(f.Cause, `"example.com/cleanup"`) {
		t.Fatalf("cause should name the finalizer, got %q", f.Cause)
	}
}

func TestTerminatingWithinGraceIsNotStuck(t *testing.T) {
	p := basePod("a")
	p.Finalizers = []string{"example.com/cleanup"}
	p.DeletionTimestamp = &metav1.Time{Time: testNow.Add(-10 * time.Second)}
	p.DeletionGracePeriodSeconds = ptr.To(int64(30))
	if f := diagnosePod(emptyCtx(), p); f != nil {
		t.Fatalf("a pod inside its grace period is not stuck, got %+v", f)
	}
}

func TestTerminatingWithoutFinalizersIsNotStuck(t *testing.T) {
	p := basePod("a")
	p.DeletionTimestamp = &metav1.Time{Time: testNow.Add(-2 * time.Minute)}
	if f := diagnosePod(emptyCtx(), p); f != nil {
		t.Fatalf("no finalizers, nothing to blame, got %+v", f)
	}
}

func TestInitContainerAttribution(t *testing.T) {
	p := basePod("a")
	p.Spec.InitContainers = []corev1.Container{{Name: "migrate", Image: "example.com/migrate:v1"}}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "migrate",
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "CrashLoopBackOff" {
		t.Fatalf("want CrashLoopBackOff on the init container, got %+v", f)
	}
	if !strings.HasPrefix(f.Cause, "init container migrate") {
		t.Fatalf("cause must attribute the failure to the init container, got %q", f.Cause)
	}
}

// TestDiagnoseOldPodStuckTerminating: previous-revision pods reach diagnosis
// through the old-pod path and get exactly the wedge question.
func TestDiagnoseOldPodStuckTerminating(t *testing.T) {
	old := basePod("old")
	old.Finalizers = []string{"example.com/cleanup"}
	// Far enough in the past for any wall clock: Diagnose uses time.Now.
	old.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	// An old pod that is also crash-looping must NOT yield a crash finding.
	old.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}

	cs := fake.NewClientset()
	rep := Diagnose(context.Background(), cs, TargetRef{Kind: "Deployment", Namespace: "ns", Name: "api"}, nil, []*corev1.Pod{old})
	if len(rep.Findings) != 1 || rep.Findings[0].Signature != "PodStuckTerminating" {
		t.Fatalf("old pod should yield exactly one stuck-terminating finding, got %+v", rep.Findings)
	}
	if rep.Findings[0].Namespace != "ns" || rep.Findings[0].Pod != "old" {
		t.Fatalf("finding must be anchored to the old pod, got %+v", rep.Findings[0])
	}
}

// TestContainerFailedClaimsJobRetryPod: a restartPolicy-Never pod that ran
// and exited non-zero has no crash loop and no restarts — the shape of every
// Job retry — and must still be diagnosed.
func TestContainerFailedClaimsJobRetryPod(t *testing.T) {
	p := basePod("retry")
	p.Status.Phase = corev1.PodFailed
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error"}},
	}}
	c := emptyCtx()
	c.CrashLogs = func(*corev1.Pod, corev1.ContainerStatus) string { return "FATAL: migration checksum mismatch\n" }
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "ContainerFailed" {
		t.Fatalf("want ContainerFailed, got %+v", f)
	}
	if !strings.Contains(f.Cause, "exited with code 5") {
		t.Fatalf("cause should carry the exit code, got %q", f.Cause)
	}
	found := false
	for _, ev := range f.Evidence {
		if ev.Source == "log" && strings.Contains(ev.Text, "checksum mismatch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the dead container's logs belong in evidence: %+v", f.Evidence)
	}
}

func TestEvictedOutranksContainerFailed(t *testing.T) {
	p := basePod("gone")
	p.Status.Phase = corev1.PodFailed
	p.Status.Reason = "Evicted"
	p.Status.Message = "The node had condition: [DiskPressure]."
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "PodEvicted" {
		t.Fatalf("eviction is the deeper cause, got %+v", f)
	}
}
