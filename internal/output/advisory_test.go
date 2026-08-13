package output

import (
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
	for _, want := range []string{"4 termination(s) in last 24h", "exit 137", "40m ago", "lifetime restarts across pods: 150"} {
		if !strings.Contains(got, want) {
			t.Fatalf("advisory %q missing %q", got, want)
		}
	}

	quiet := []*corev1.Pod{restartyPod(6, 1, now.Add(-27*24*time.Hour))}
	if got := RecentRestarts(quiet, 24*time.Hour, now); got != "" {
		t.Fatalf("27-day-old crash must stay quiet, got %q", got)
	}

	if got := RecentRestarts(loud, 0, now); got != "" {
		t.Fatalf("zero window disables the advisory, got %q", got)
	}

	never := []*corev1.Pod{{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main"}}}}}
	if got := RecentRestarts(never, 24*time.Hour, now); got != "" {
		t.Fatalf("no terminations must stay quiet, got %q", got)
	}
}
