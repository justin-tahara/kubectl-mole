package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func restartyPod(restarts int32, lastExit int32, finished time.Time) *corev1.Pod {
	return &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "main",
				RestartCount: restarts,
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   lastExit,
						FinishedAt: metav1.Time{Time: finished},
					},
				},
			}},
		},
	}
}

// The two shapes from the dogfood evidence: same-day liveness kills must
// speak; a many-restart pod whose last crash is weeks old must stay quiet.
func TestRecentRestartsGate(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)

	loud := []*corev1.Pod{
		restartyPod(98, 137, now.Add(-6*time.Hour)),
		restartyPod(31, 137, now.Add(-40*time.Minute)),
		restartyPod(1, 137, now.Add(-50*time.Minute)),
		restartyPod(20, 137, now.Add(-3*time.Hour)),
	}
	got := RecentRestarts(loud, 24*time.Hour, now)
	if got == nil {
		t.Fatal("same-day terminations must produce an advisory")
	}
	if got.Kind != "recent-restarts" || got.TerminationsInWindow != 4 || got.Window != "24h" ||
		got.LastExitCode == nil || *got.LastExitCode != 137 ||
		got.LastTerminatedAgo != "40m" || got.LifetimeRestarts != 150 {
		t.Fatalf("structured fields wrong: %+v", got)
	}
	for _, want := range []string{"4 termination(s) in last 24h", "exit 137", "40m ago", "lifetime restarts across pods: 150"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("text %q missing %q", got.Text, want)
		}
	}

	quiet := []*corev1.Pod{restartyPod(6, 1, now.Add(-27*24*time.Hour))}
	if got := RecentRestarts(quiet, 24*time.Hour, now); got != nil {
		t.Fatalf("27-day-old crash must stay quiet, got %+v", got)
	}

	if got := RecentRestarts(loud, 0, now); got != nil {
		t.Fatalf("zero window disables the advisory, got %+v", got)
	}

	never := []*corev1.Pod{{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main"}}}}}
	if got := RecentRestarts(never, 24*time.Hour, now); got != nil {
		t.Fatalf("no terminations must stay quiet, got %+v", got)
	}
}

// The JSON field names are the machine interface fleet automation binds
// to (issue #34): rank on terminationsInWindow, filter on lastExitCode,
// locate by context/target — never regex the display text.
func TestAdvisoryJSONShape(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	adv := RecentRestarts([]*corev1.Pod{restartyPod(5, 137, now.Add(-2*time.Hour))}, 24*time.Hour, now)
	adv.Context = "mane-a"
	adv.Target = "Deployment/api-server"
	adv.Namespace = "app"

	b, err := json.Marshal(adv)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"kind": "recent-restarts", "context": "mane-a",
		"target": "Deployment/api-server", "namespace": "app",
		"terminationsInWindow": float64(1), "window": "24h",
		"lastExitCode": float64(137), "lastTerminatedAgo": "2h",
		"lifetimeRestarts": float64(5),
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("json field %q = %v, want %v (full: %s)", k, m[k], v, b)
		}
	}
	if _, ok := m["text"].(string); !ok {
		t.Fatalf("text field missing: %s", b)
	}

	// A single-target advisory omits the locator fields entirely.
	bare, _ := json.Marshal(RecentRestarts([]*corev1.Pod{restartyPod(5, 137, now.Add(-2*time.Hour))}, 24*time.Hour, now))
	for _, absent := range []string{"context", "target", "namespace"} {
		if strings.Contains(string(bare), `"`+absent+`"`) {
			t.Fatalf("single-target advisory must omit %q: %s", absent, bare)
		}
	}
}

// A zero exit code must survive to the JSON: absent means "not this
// advisory kind", never "exited zero".
func TestAdvisoryZeroExitCodeEmitted(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	adv := RecentRestarts([]*corev1.Pod{restartyPod(3, 0, now.Add(-time.Hour))}, 24*time.Hour, now)
	b, _ := json.Marshal(adv)
	if !strings.Contains(string(b), `"lastExitCode":0`) {
		t.Fatalf("exit 0 dropped from JSON: %s", b)
	}
}
