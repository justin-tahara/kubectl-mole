package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Issue #21: a verdict blocked on previous-revision pods said
// "pods: 0/0 ready" while its reason counted three of them. The summary
// now accounts for old pods — additively, so verdicts without them are
// byte-identical.
func TestSummaryCountsOldPods(t *testing.T) {
	oldPod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	in := Input{
		Kind: "DaemonSet", Name: "agent", Namespace: "kube-system",
		Status: StatusProgressing, Reason: "3 pod(s) from previous revisions still present",
		Elapsed: 46 * time.Second,
		OldPods: []*corev1.Pod{oldPod("a"), oldPod("b"), oldPod("c")},
	}
	v := Build(in)
	if v.Summary.Old != 3 || v.Summary.Total != 0 {
		t.Fatalf("summary = %+v, want old=3 total=0", v.Summary)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"old":3`) {
		t.Fatalf("json missing old count: %s", b)
	}

	var text strings.Builder
	WriteText(&text, v, nil)
	if !strings.Contains(text.String(), "0/0 ready, 0 failed (3 previous-revision still present)") {
		t.Fatalf("text must account for the old pods, got:\n%s", text.String())
	}

	in.OldPods = nil
	b, _ = json.Marshal(Build(in))
	if strings.Contains(string(b), `"old"`) {
		t.Fatalf("old must be omitted when zero: %s", b)
	}
}
