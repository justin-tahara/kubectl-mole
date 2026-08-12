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

// NoMatchError means a fan-out selection matched no workloads. The CLI maps
// it to the no_resources_matched verdict (exit 4), like NotFoundError.
type NoMatchError struct {
	Selector  string
	Namespace string // "" means all namespaces
}

func (e *NoMatchError) Error() string {
	scope := "namespace " + e.Namespace
	if e.Namespace == "" {
		scope = "any namespace"
	}
	if e.Selector == "" {
		return fmt.Sprintf("no workloads found in %s", scope)
	}
	return fmt.Sprintf("no workloads match selector %q in %s", e.Selector, scope)
}

// OverCeilingError means a fan-out selection matched more workloads than the
// ceiling. Watching an unbounded fleet hurts the API server and the caller's
// context window alike, so the run is refused before any watch starts.
// Matched is a lower bound: discovery stops counting past the ceiling.
type OverCeilingError struct {
	Matched int
	Ceiling int
}

func (e *OverCeilingError) Error() string {
	return fmt.Sprintf("selection matches at least %d workloads, above the --max-targets ceiling of %d; narrow the selector or raise --max-targets",
		e.Matched, e.Ceiling)
}

// PermissionError means RBAC denies a read the watch depends on. The message
// names the missing verb and resource; a raw 403 never reaches the user. The
// CLI maps it to the permission_denied verdict (exit 3).
type PermissionError struct {
	Verb     string
	Resource string
	// Namespace is empty for cluster-scope reads (--all-namespaces).
	Namespace string
}

func (e *PermissionError) Error() string {
	if e.Namespace == "" {
		return fmt.Sprintf("cannot %s %s at the cluster scope", e.Verb, e.Resource)
	}
	return fmt.Sprintf("cannot %s %s in namespace %s", e.Verb, e.Resource, e.Namespace)
}
