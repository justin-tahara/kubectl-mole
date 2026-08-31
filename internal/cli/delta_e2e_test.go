//go:build e2e

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/justin-tahara/kubectl-mole/internal/output"
)

// Issue #46 end to end: the incident loop. Run 1 is the baseline with normal
// exit codes; run 2 finds nothing moved and exits 0; then the container is
// killed again and recovers before run 3 — the settle-state hash cannot see
// that (advisories are excluded from it), and the advisory diff must.
func TestDeltaIncidentLoop(t *testing.T) {
	t.Parallel()
	cs, kubeconfig := e2eSetup(t)
	ns := e2eNamespace(t, cs)

	e2eDeployment(t, cs, ns, "flappy", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name:         "state",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		d.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "state", MountPath: "/state"}}
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c",
			"test -f /state/crashed || { touch /state/crashed; exit 7; }; sleep 3600"}
	})

	state := filepath.Join(t.TempDir(), "last.json")
	save := func(v output.Verdict) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(state, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(timeout string) (output.Verdict, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		return runMole(t, ctx, "deployment/flappy", "-n", ns,
			"--kubeconfig", kubeconfig, "-o", "json", "--timeout", timeout, "--stable-for", "5s",
			"--since", state)
	}

	// The baseline's advisory is the whole premise of the loop, so the
	// crash has to be recorded before the first check.
	e2eAwaitTermination(t, cs, ns, "flappy", time.Time{})

	// Run 1: no state file yet — the baseline, with normal exit codes.
	v1, exit := run("2m")
	if v1.Status != output.StatusSettled || exit != 0 {
		t.Fatalf("baseline must settle, got %s/%d (%s)", v1.Status, exit, v1.Reason)
	}
	if v1.Delta == nil || !v1.Delta.Baseline {
		t.Fatalf("first run must mark itself the baseline, got %+v", v1.Delta)
	}
	if len(v1.Advisories) != 1 || v1.Advisories[0].LastFinishedAt == "" {
		t.Fatalf("crash-once advisory must carry lastFinishedAt, got %+v", v1.Advisories)
	}
	save(v1)

	// Run 2: nothing moved. Exit 0, no transitions.
	v2, exit := run("2m")
	if exit != 0 || v2.Delta == nil || v2.Delta.Changed || len(v2.Delta.Transitions) != 0 {
		t.Fatalf("quiet run must exit 0 with no transitions, got exit %d delta %+v", exit, v2.Delta)
	}
	if v2.ContentHash != v1.ContentHash {
		t.Fatalf("test premise: settle state must hash identically, got %s vs %s", v2.ContentHash, v1.ContentHash)
	}
	save(v2)

	// The event the loop watches for: the container is killed again and
	// recovers before the next check (a fresh pod crashes once and holds).
	killedAt := time.Now()
	if err := cs.CoreV1().Pods(ns).DeleteCollection(context.Background(), metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: "app=flappy"}); err != nil {
		t.Fatal(err)
	}
	// "recovers before the next check" is the point of run 3: wait for the
	// replacement pod's own crash, not the one run 1 already saw.
	e2eAwaitTermination(t, cs, ns, "flappy", killedAt)

	// Run 3: the settle-state hash still matches — pod names are not part
	// of a settled verdict — but the advisory's freshest termination moved.
	v3, exit := run("3m")
	if v3.Status != output.StatusSettled {
		t.Fatalf("recovered deployment must settle, got %s (%s)", v3.Status, v3.Reason)
	}
	if v3.ContentHash != v2.ContentHash {
		t.Fatalf("premise: kill-and-recover must be hash-invisible, got %s vs %s", v3.ContentHash, v2.ContentHash)
	}
	if exit != output.ExitChanged {
		t.Fatalf("kill-and-recover must exit %d, got %d (delta %+v)", output.ExitChanged, exit, v3.Delta)
	}
	if len(v3.Delta.Transitions) != 1 || v3.Delta.Transitions[0].Kind != output.TransitionNewTermination {
		t.Fatalf("want exactly the new-termination transition, got %+v", v3.Delta.Transitions)
	}
	if v3.Delta.Transitions[0].LastFinishedAt == v1.Advisories[0].LastFinishedAt {
		t.Fatalf("the transition must carry the NEW termination's time, got %+v", v3.Delta.Transitions[0])
	}
	t.Logf("run3 transition: %s", v3.Delta.Transitions[0].Text)
}
