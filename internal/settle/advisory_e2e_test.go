//go:build e2e

package settle_test

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/justin-tahara/kubectl-mole/internal/output"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

// Issue #22's shape at e2e scale: a container that crashed and recovered
// settles — correctly — and the advisory must surface the fresh
// termination evidence a real kubelet records, while an unrestarted
// workload stays quiet.
func TestSettledVerdictCarriesRecentRestartAdvisory(t *testing.T) {
	t.Parallel()
	cs := client(t)
	ns := testNamespace(t, cs)

	// Crash exactly once: the marker survives the container restart on the
	// pod's emptyDir, so the second start holds.
	create(t, cs, ns, newDeployment("flappy", 1, func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "state",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		d.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "state", MountPath: "/state"}}
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c",
			"test -f /state/crashed || { touch /state/crashed; exit 7; }; sleep 3600"}
	}))

	res := runSettle(t, cs, ns, "flappy", 90*time.Second, 5*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("crash-once deployment must settle, got %s (%s)", res.Outcome, res.Reason)
	}
	adv := output.RecentRestarts(res.Final.CurrentPods, 24*time.Hour, time.Now())
	if adv == "" {
		t.Fatal("settled verdict must carry the fresh termination advisory")
	}
	if !strings.Contains(adv, "exit 7") || !strings.Contains(adv, "1 termination(s)") {
		t.Fatalf("advisory should name the crash, got %q", adv)
	}
	t.Logf("advisory: %s", adv)

	// The quiet side: a workload that never restarted says nothing.
	create(t, cs, ns, newDeployment("calm", 1))
	res = runSettle(t, cs, ns, "calm", 60*time.Second, 5*time.Second)
	if res.Outcome != settle.OutcomeSettled {
		t.Fatalf("calm deployment must settle, got %s (%s)", res.Outcome, res.Reason)
	}
	if adv := output.RecentRestarts(res.Final.CurrentPods, 24*time.Hour, time.Now()); adv != "" {
		t.Fatalf("unrestarted workload must stay quiet, got %q", adv)
	}
}
