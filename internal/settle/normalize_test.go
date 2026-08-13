package settle

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func genWidget(observed any) *unstructured.Unstructured {
	st := map[string]any{}
	if observed != nil {
		st["observedGeneration"] = observed
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "mole.example.com/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "w", "generation": int64(2)},
		"status":     st,
	}}
}

// The two string shapes Argo Rollouts has published: a numeric string keeps
// the staleness guard working, a legacy hash drops the field so kstatus
// skips the check instead of erroring out of the whole watch.
func TestNormalizeObservedGeneration(t *testing.T) {
	got, found, _ := unstructured.NestedInt64(normalizeObservedGeneration(genWidget("2")).Object, "status", "observedGeneration")
	if !found || got != 2 {
		t.Fatalf("numeric string must coerce to int64 2, got %v found=%v", got, found)
	}

	_, found, _ = unstructured.NestedFieldNoCopy(normalizeObservedGeneration(genWidget("7d9fabc")).Object, "status", "observedGeneration")
	if found {
		t.Fatalf("a hash observedGeneration must be dropped")
	}

	u := genWidget(int64(2))
	if normalizeObservedGeneration(u) != u {
		t.Fatalf("an int64 field must pass through without a copy")
	}
	u = genWidget(nil)
	if normalizeObservedGeneration(u) != u {
		t.Fatalf("an absent field must pass through without a copy")
	}
}

// The informer cache owns the object: normalization must never write
// through to it.
func TestNormalizeObservedGenerationLeavesCacheUntouched(t *testing.T) {
	u := genWidget("2")
	_ = normalizeObservedGeneration(u)
	raw, _, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "observedGeneration")
	if s, ok := raw.(string); !ok || s != "2" {
		t.Fatalf("original object mutated: %v", raw)
	}
}
