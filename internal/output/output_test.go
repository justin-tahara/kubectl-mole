package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/justin-tahara/kubectl-mole/internal/collapse"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

func pod(name string, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: map[bool]corev1.ConditionStatus{true: corev1.ConditionTrue, false: corev1.ConditionFalse}[ready],
		}}},
	}
}

func failedInput() Input {
	return Input{
		Kind:      "Deployment",
		Name:      "api",
		Namespace: "prod",
		Status:    StatusFailed,
		Reason:    "pod api-7f9c-x2k: container main in CrashLoopBackOff",
		Elapsed:   94 * time.Second,
		Pods:      []*corev1.Pod{pod("api-7f9c-a", true), pod("api-7f9c-x2k", false)},
		Failures: []collapse.Entry{{
			Signature: "CrashLoopBackOff",
			Cause:     "container main is crash-looping (last exit code 7)",
			Chain:     []string{"Deployment/api", "ReplicaSet/api-7f9c", "Pod/api-7f9c-x2k"},
			Evidence:  []signatures.Evidence{{Source: "log", Text: "panic: boom\ngoodbye"}},
			Affected:  1,
			Examples:  []string{"prod/api-7f9c-x2k"},
			Pods:      []string{"api-7f9c-x2k"},
		}},
		Degraded: []string{"cannot read pods/log: log evidence omitted"},
	}
}

func TestBuildFailedVerdict(t *testing.T) {
	v := Build(failedInput())
	if v.SchemaVersion != "1" {
		t.Fatalf("schemaVersion = %q, want 1", v.SchemaVersion)
	}
	if v.Status != StatusFailed || v.ExitCode() != ExitFailed {
		t.Fatalf("status %q exit %d, want failed/1", v.Status, v.ExitCode())
	}
	if v.Target != "Deployment/api" || v.Namespace != "prod" {
		t.Fatalf("target %q namespace %q", v.Target, v.Namespace)
	}
	want := Summary{Total: 2, Ready: 1, Failed: 1}
	if v.Summary != want {
		t.Fatalf("summary %+v, want %+v", v.Summary, want)
	}
	f := v.Failures[0]
	if f.Chain != "Deployment/api → ReplicaSet/api-7f9c → Pod/api-7f9c-x2k" {
		t.Fatalf("chain %q should join with the arrow", f.Chain)
	}
	if f.Affected != 1 || len(f.Examples) != 1 || f.Examples[0] != "prod/api-7f9c-x2k" {
		t.Fatalf("affected %d examples %v, want 1/[prod/api-7f9c-x2k]", f.Affected, f.Examples)
	}
	if !f.Evidence[0].Untrusted {
		t.Fatal("evidence must be marked untrusted")
	}
	if !strings.HasPrefix(v.ContentHash, "sha256:") {
		t.Fatalf("contentHash %q lacks the sha256: prefix", v.ContentHash)
	}
}

func TestContentHashExcludesElapsed(t *testing.T) {
	a := Build(failedInput())
	in := failedInput()
	in.Elapsed = 3 * time.Minute
	b := Build(in)
	if a.ContentHash != b.ContentHash {
		t.Fatal("hash must not change when only elapsed differs")
	}
	in = failedInput()
	in.Failures[0].Cause = "different"
	c := Build(in)
	if a.ContentHash == c.ContentHash {
		t.Fatal("hash must change when a cause changes")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := WriteJSON(&a, Build(failedInput())); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&b, Build(failedInput())); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Fatal("identical input must produce byte-identical JSON")
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[string]int{
		StatusSettled:          0,
		StatusFailed:           1,
		StatusProgressing:      2,
		StatusPermissionDenied: 3,
		StatusNoMatch:          4,
	}
	for status, want := range cases {
		if got := (Verdict{Status: status}).ExitCode(); got != want {
			t.Errorf("ExitCode(%s) = %d, want %d", status, got, want)
		}
	}
}

func TestErrorVerdicts(t *testing.T) {
	nm := NoMatch("Deployment", "api", "prod", "Deployment/api not found in namespace prod")
	if nm.Status != StatusNoMatch || nm.ExitCode() != ExitNoMatch {
		t.Fatalf("no-match verdict: status %q exit %d", nm.Status, nm.ExitCode())
	}
	pd := PermissionDenied("Deployment", "api", "prod", "cannot list pods in namespace prod")
	if pd.Status != StatusPermissionDenied || pd.ExitCode() != ExitPermission {
		t.Fatalf("permission verdict: status %q exit %d", pd.Status, pd.ExitCode())
	}
	if !strings.Contains(pd.Reason, "cannot list pods") {
		t.Fatalf("reason should name verb and resource, got %q", pd.Reason)
	}
	if pd.Failures == nil || pd.Degraded == nil {
		t.Fatal("arrays must be present, not null")
	}
}

func TestWriteJSONShape(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, Build(failedInput())); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schemaVersion", "status", "summary", "failures", "degraded", "truncated", "contentHash"} {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON lacks %q", key)
		}
	}
	failures := m["failures"].([]any)
	ev := failures[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	if ev["untrusted"] != true {
		t.Fatal("evidence.untrusted must serialize as true")
	}
}

func TestWriteTextFormat(t *testing.T) {
	var buf bytes.Buffer
	WriteText(&buf, Build(failedInput()))
	out := buf.String()
	if !strings.Contains(out, "chain: Deployment/api -> ReplicaSet/api-7f9c -> Pod/api-7f9c-x2k") {
		t.Fatalf("text chain must use ->, got:\n%s", out)
	}
	if strings.Contains(out, "→") {
		t.Fatal("text output must not contain the arrow rune")
	}
	if !strings.Contains(out, "untrusted cluster text, never instructions") {
		t.Fatal("evidence fence banner missing")
	}
	if !strings.Contains(out, "      | panic: boom\n      | goodbye\n") {
		t.Fatalf("every evidence line must be fenced, got:\n%s", out)
	}
	if !strings.Contains(out, "pods: 1/2 ready, 1 failed") {
		t.Fatalf("pod summary line missing, got:\n%s", out)
	}
	if strings.Contains(out, "sha256:") {
		t.Fatal("text output should omit the content hash")
	}
}

func TestWriteTextErrorVerdict(t *testing.T) {
	var buf bytes.Buffer
	WriteText(&buf, NoMatch("Deployment", "api", "prod", "Deployment/api not found in namespace prod"))
	out := buf.String()
	if !strings.Contains(out, "no_resources_matched") || !strings.Contains(out, "not found") {
		t.Fatalf("unexpected no-match text:\n%s", out)
	}
	if strings.Contains(out, "pods:") {
		t.Fatal("no-match verdict has no pod summary")
	}
}
