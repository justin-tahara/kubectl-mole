package settle

import "fmt"

// NotFoundError means the target does not exist. The CLI maps it to the
// no_resources_matched verdict (exit 4): kubectl exits 0 when a selector
// matches nothing, and consumers read that as success.
type NotFoundError struct {
	Target Target
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s not found in namespace %s", e.Target, e.Target.Namespace)
}

// PermissionError means RBAC denies a read the watch depends on. The message
// names the missing verb and resource; a raw 403 never reaches the user. The
// CLI maps it to the permission_denied verdict (exit 3).
type PermissionError struct {
	Verb      string
	Resource  string
	Namespace string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("cannot %s %s in namespace %s", e.Verb, e.Resource, e.Namespace)
}
