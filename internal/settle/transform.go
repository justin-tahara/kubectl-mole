package settle

import "k8s.io/apimachinery/pkg/api/meta"

// stripManagedFields drops server-side-apply bookkeeping from every object
// before it enters an informer cache — the single largest per-object
// metadata payload, and nothing in mole reads it. Objects the accessor does
// not understand (cache tombstones) pass through untouched: a transform
// must never error on them.
func stripManagedFields(obj any) (any, error) {
	if acc, err := meta.Accessor(obj); err == nil {
		acc.SetManagedFields(nil)
	}
	return obj, nil
}
