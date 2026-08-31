//go:build e2e

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/justin-tahara/kubectl-mole/internal/output"
)

// Issue #45 end to end: a workload that crashed once and recovered settles
// with an advisory, and the advisory carries the previous instance's log
// tail — the artifact that explains the crash, fetched while it still
// exists — clipped and marked untrusted exactly like failure evidence.
func TestSettledAdvisoryCarriesCrashLogEvidence(t *testing.T) {
	t.Parallel()
	cs, kubeconfig := e2eSetup(t)
	ns := e2eNamespace(t, cs)

	const marker = "FATAL: allocation walked through the limit"
	e2eDeployment(t, cs, ns, "flappy", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "state",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		d.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "state", MountPath: "/state"}}
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c",
			"test -f /state/crashed || { touch /state/crashed; echo '" + marker + "'; exit 7; }; sleep 3600"}
	})

	// The crash must be in the pod's status before mole settles, or there
	// is no advisory to carry evidence.
	e2eAwaitTermination(t, cs, ns, "flappy", time.Time{})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	v, exit := runMole(t, ctx, "deployment/flappy", "-n", ns,
		"--kubeconfig", kubeconfig, "-o", "json", "--timeout", "2m", "--stable-for", "5s")

	if v.Status != output.StatusSettled || exit != 0 {
		t.Fatalf("crash-once deployment must settle, got %s/%d (%s)", v.Status, exit, v.Reason)
	}
	if len(v.Advisories) != 1 {
		t.Fatalf("settled verdict must carry the restart advisory, got %+v", v.Advisories)
	}
	adv := v.Advisories[0]
	if adv.LastReason != "Error" || adv.LastExitCode == nil || *adv.LastExitCode != 7 {
		t.Fatalf("advisory must name the crash, got %+v", adv)
	}
	if len(adv.Evidence) != 1 {
		t.Fatalf("advisory must carry the previous-instance log tail, got %+v", adv)
	}
	ev := adv.Evidence[0]
	if ev.Source != "log" || !ev.Untrusted {
		t.Fatalf("advisory evidence must be a log marked untrusted, got %+v", ev)
	}
	if !strings.Contains(ev.Text, marker) {
		t.Fatalf("evidence must contain the dead container's final lines, got %q", ev.Text)
	}
	t.Logf("advisory: %s", adv.Text)
	t.Logf("evidence: %s", ev.Text)
}
