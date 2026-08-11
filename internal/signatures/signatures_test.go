package signatures

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func resourceMustParse(t *testing.T, s string) resource.Quantity {
	t.Helper()
	return resource.MustParse(s)
}

func intstrFromInt(i int32) intstr.IntOrString { return intstr.FromInt32(i) }

// emptyCtx is a Context whose reads all come back empty — the fully degraded
// case detectors must survive.
func emptyCtx() *Context {
	return &Context{
		PodEvents:    func(*corev1.Pod) []corev1.Event { return nil },
		PVC:          func(string) *corev1.PersistentVolumeClaim { return nil },
		PVCEvents:    func(string) []corev1.Event { return nil },
		PreviousLogs: func(*corev1.Pod, string) string { return "" },
	}
}

func basePod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "example.com/app:v1"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

// diagnosePod runs the priority-ordered detector chain the way Diagnose does.
func diagnosePod(c *Context, p *corev1.Pod) *Finding {
	for _, det := range podDetectors {
		if f := det.detect(c, p); f != nil {
			return f
		}
	}
	return nil
}

func TestImagePullAuthImplicated(t *testing.T) {
	p := basePod("a")
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied for example.com/app"}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "ImagePullBackOff" {
		t.Fatalf("want ImagePullBackOff, got %+v", f)
	}
	if !strings.Contains(f.Cause, `"example.com/app:v1"`) || !strings.Contains(f.Cause, "registry auth implicated") {
		t.Fatalf("cause should name the image and implicate auth, got %q", f.Cause)
	}
}

func TestCrashLoopAttachesLogsAndExitCode(t *testing.T) {
	p := basePod("a")
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "main",
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 7}},
	}}
	c := emptyCtx()
	c.PreviousLogs = func(*corev1.Pod, string) string { return "panic: boom\n" }
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "CrashLoopBackOff" {
		t.Fatalf("want CrashLoopBackOff, got %+v", f)
	}
	if !strings.Contains(f.Cause, "exit code 7") {
		t.Fatalf("cause should carry the exit code, got %q", f.Cause)
	}
	found := false
	for _, ev := range f.Evidence {
		if ev.Source == "log" && strings.Contains(ev.Text, "panic: boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("previous logs missing from evidence: %+v", f.Evidence)
	}
}

func TestCrashLoopSurvivesDeniedLogs(t *testing.T) {
	p := basePod("a")
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "CrashLoopBackOff" {
		t.Fatalf("detector must fire without log evidence, got %+v", f)
	}
	for _, ev := range f.Evidence {
		if ev.Source == "log" {
			t.Fatalf("unexpected log evidence: %+v", ev)
		}
	}
}

func TestCrashLoopNotesLivenessProbe(t *testing.T) {
	p := basePod("a")
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	c := emptyCtx()
	c.PodEvents = func(*corev1.Pod) []corev1.Event {
		return []corev1.Event{{Reason: "Unhealthy", Message: "Liveness probe failed: HTTP probe failed with statuscode: 500"}}
	}
	f := diagnosePod(c, p)
	if f == nil || !strings.Contains(f.Cause, "liveness probe") {
		t.Fatalf("cause should link restarts to the liveness probe, got %+v", f)
	}
}

func TestOOMKilledOutranksCrashLoop(t *testing.T) {
	p := basePod("a")
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: resourceMustParse(t, "16Mi"),
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 "main",
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "OOMKilled" {
		t.Fatalf("OOMKilled must outrank CrashLoopBackOff, got %+v", f)
	}
	if !strings.Contains(f.Cause, "16Mi") {
		t.Fatalf("cause should carry the memory limit, got %q", f.Cause)
	}
}

func TestPVCPendingOutranksUnschedulable(t *testing.T) {
	p := basePod("a")
	p.Spec.Volumes = []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
	}}
	p.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/1 nodes are available: pod has unbound immediate PersistentVolumeClaims.",
	}}
	c := emptyCtx()
	c.PVC = func(name string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("gp3-encrypted")},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}
	}
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "PVCPending" {
		t.Fatalf("the pending PVC is the cause, not the unschedulable symptom; got %+v", f)
	}
	if !strings.Contains(f.Cause, "gp3-encrypted") {
		t.Fatalf("cause should carry the storageClass, got %q", f.Cause)
	}
}

func TestUnschedulableCarriesPredicates(t *testing.T) {
	p := basePod("a")
	p.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: 3 Insufficient cpu.",
	}}
	f := diagnosePod(emptyCtx(), p)
	if f == nil || f.Signature != "PodUnschedulable" {
		t.Fatalf("want PodUnschedulable, got %+v", f)
	}
	if !strings.Contains(f.Cause, "Insufficient cpu") {
		t.Fatalf("cause should carry the scheduler predicate, got %q", f.Cause)
	}
}

func TestProbeFailingNamesProbeContainerEndpoint(t *testing.T) {
	p := basePod("a")
	p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstrFromInt(8080)}},
	}
	p.Status.Phase = corev1.PodRunning
	c := emptyCtx()
	c.PodEvents = func(*corev1.Pod) []corev1.Event {
		return []corev1.Event{{
			Reason:         "Unhealthy",
			Message:        "Readiness probe failed: HTTP probe failed with statuscode: 500",
			InvolvedObject: corev1.ObjectReference{FieldPath: "spec.containers{main}"},
		}}
	}
	f := diagnosePod(c, p)
	if f == nil || f.Signature != "ProbeFailing" {
		t.Fatalf("want ProbeFailing, got %+v", f)
	}
	for _, want := range []string{"readiness", "main", "/healthz", "8080"} {
		if !strings.Contains(f.Cause, want) {
			t.Fatalf("cause %q should contain %q", f.Cause, want)
		}
	}
}

func TestMatchAdmission(t *testing.T) {
	msg := `Error creating: admission webhook "deny.example.com" denied the request: images must come from the internal registry`
	f := matchAdmission(msg)
	if f == nil || f.Signature != "AdmissionRejected" {
		t.Fatalf("want AdmissionRejected, got %+v", f)
	}
	if !strings.Contains(f.Cause, "deny.example.com") || !strings.Contains(f.Cause, "internal registry") {
		t.Fatalf("cause should carry webhook name and rejection message, got %q", f.Cause)
	}
	if matchAdmission("Error creating: pods \"x\" is forbidden: exceeded quota: q, requested: pods=1, used: pods=0, limited: pods=0") != nil {
		t.Fatal("quota message must not match the admission detector")
	}
}

func TestMatchQuota(t *testing.T) {
	msg := `Error creating: pods "api-x" is forbidden: exceeded quota: compute-resources, requested: pods=1, used: pods=0, limited: pods=0`
	f := matchQuota(msg)
	if f == nil || f.Signature != "QuotaExceeded" {
		t.Fatalf("want QuotaExceeded, got %+v", f)
	}
	if !strings.Contains(f.Cause, "compute-resources") || !strings.Contains(f.Cause, "pods=1") {
		t.Fatalf("cause should carry quota name and requested resource, got %q", f.Cause)
	}
}

// TestDiagnoseWiring drives Diagnose against a fake clientset: chain built
// from the pod's controller ref, event indexed by UID, one finding per pod.
func TestDiagnoseWiring(t *testing.T) {
	pod := basePod("api-7f9c-x2k")
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "api-7f9c", UID: "rs-uid", Controller: ptr.To(true),
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
	}}
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev1", Namespace: "ns"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, UID: pod.UID},
		Reason:         "Failed",
		Message:        `Failed to pull image "example.com/app:v1": not found`,
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns", UID: "dep-uid"}}

	cs := fake.NewSimpleClientset(pod, event, dep)
	rep := Diagnose(context.Background(), cs, TargetRef{Kind: "Deployment", Namespace: "ns", Name: "api"}, []*corev1.Pod{pod})

	if len(rep.Findings) != 1 {
		t.Fatalf("want 1 finding, got %+v", rep.Findings)
	}
	f := rep.Findings[0]
	if f.Signature != "ImagePullBackOff" {
		t.Fatalf("want ImagePullBackOff, got %q", f.Signature)
	}
	wantChain := []string{"Deployment/api", "ReplicaSet/api-7f9c", "Pod/api-7f9c-x2k"}
	if len(f.Chain) != 3 || f.Chain[0] != wantChain[0] || f.Chain[1] != wantChain[1] || f.Chain[2] != wantChain[2] {
		t.Fatalf("chain mismatch: got %v want %v", f.Chain, wantChain)
	}
	foundEvent := false
	for _, ev := range f.Evidence {
		if ev.Source == "event" && strings.Contains(ev.Text, "Failed to pull image") {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("pull event missing from evidence: %+v", f.Evidence)
	}
}
