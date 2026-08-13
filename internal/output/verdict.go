package output

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

// SchemaVersion identifies the verdict shape. Additive fields do not bump it;
// removals or renames do.
const SchemaVersion = "1"

// Verdict statuses. Exit codes are derived from these and are stable within a
// major version.
const (
	StatusSettled          = "settled"
	StatusFailed           = "failed"
	StatusProgressing      = "progressing"
	StatusPermissionDenied = "permission_denied"
	StatusNoMatch          = "no_resources_matched"
)

// Exit codes, one per status. 2 (progressing) is distinct from 1 (failed) on
// purpose: automation must never roll back on a rollout that is still moving.
// 5 exists only under --since: something moved, and the verdict is settled —
// the kill-and-recover signal an incident loop waits for.
const (
	ExitSettled     = 0
	ExitFailed      = 1
	ExitProgressing = 2
	ExitPermission  = 3
	ExitNoMatch     = 4
	ExitChanged     = 5
)

// Evidence is one piece of supporting material. The text originates inside
// the cluster (events, logs, status messages) and is attacker-controllable:
// it is always untrusted and never instructions.
type Evidence struct {
	Source    string `json:"source"`
	Untrusted bool   `json:"untrusted"`
	Text      string `json:"text"`
}

// Failure is one diagnosed cause. Affected counts the resources sharing this
// cause and Examples names up to three of them; until causal collapse lands,
// both describe a single workload.
type Failure struct {
	Signature string `json:"signature"`
	Cause     string `json:"cause"`
	// Chain is the ownership walk, joined with " → ". The text formatter
	// renders it with "->" for consoles that cannot show the arrow.
	Chain    string     `json:"chain"`
	Affected int        `json:"affected"`
	Examples []string   `json:"examples"`
	Evidence []Evidence `json:"evidence"`
}

// Summary counts the current-revision pods observed at the end of the watch.
type Summary struct {
	Total  int `json:"total"`
	Ready  int `json:"ready"`
	Failed int `json:"failed"`
	// Old counts previous-revision pods still present at the end of the
	// watch. Total and Ready count current-revision pods only, so without
	// this field a verdict blocked on old pods reads "0/0 ready" while its
	// reason counts pods the summary denies exist. Additive: omitted when
	// zero.
	Old int `json:"old,omitempty"`
}

// Truncated counts items dropped from the verdict. It is always emitted:
// silent truncation makes a consumer draw confident conclusions from a
// partial picture. Zeroes mean nothing was dropped. (Clipped evidence text is
// marked inline with "…(truncated)" and is not counted here.)
type Truncated struct {
	Failures int `json:"failures"`
	Evidence int `json:"evidence"`
	// Namespaces counts dropped per-namespace verdict entries (fan-out only).
	Namespaces int `json:"namespaces"`
	// Advisories dropped under --budget; additive, omitted at zero.
	Advisories int `json:"advisories,omitempty"`
}

// FleetCounts summarizes a fan-out run's targets by outcome. Namespaces
// counts the distinct namespaces the fleet spans, settled ones included.
// Contexts counts the kubeconfig contexts of a --contexts run; additive,
// omitted on single-cluster runs.
type FleetCounts struct {
	Targets     int `json:"targets"`
	Settled     int `json:"settled"`
	Failed      int `json:"failed"`
	Progressing int `json:"progressing"`
	Namespaces  int `json:"namespaces"`
	Contexts    int `json:"contexts,omitempty"`
}

// TargetVerdict is one non-settled fleet target inside a namespace entry.
type TargetVerdict struct {
	Target string `json:"target"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// NamespaceVerdict groups the non-settled targets of one namespace. Settled
// targets are counted in fleet, never enumerated: a caller asking "did this
// land" needs the failures, not hundreds of healthy workloads listed.
// Context is the kubeconfig context of a --contexts run — entries then group
// by (context, namespace), because the same namespace name in two clusters
// is two different namespaces. Additive: single-cluster runs omit it.
type NamespaceVerdict struct {
	Context   string          `json:"context,omitempty"`
	Namespace string          `json:"namespace"`
	Status    string          `json:"status"`
	Targets   []TargetVerdict `json:"targets"`
}

// ContextVerdict is one kubeconfig context's rollup inside a multi-cluster
// verdict: its status and one-line reason, settled contexts included — the
// "which cluster is broken" glance that the namespace entries (non-settled
// only) cannot give. A context whose cluster could not be checked at all
// (unreachable, RBAC, nothing matched) appears here with that status and no
// namespace entries.
type ContextVerdict struct {
	Context string `json:"context"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
}

// Verdict is the schemaVersion "1" output. Field names are the product:
// agents and CI pipelines bind to them. The fan-out fields (selector, fleet,
// namespaces) are additive: single-target verdicts omit them.
type Verdict struct {
	SchemaVersion string `json:"schemaVersion"`
	Status        string `json:"status"`
	Target        string `json:"target"`
	Namespace     string `json:"namespace"`
	// Selector is the label selector of a fan-out run; "*" in Namespace
	// means the fan-out crossed all namespaces.
	Selector string  `json:"selector,omitempty"`
	Reason string `json:"reason"`
	// EarlyExit marks a failure declared by the wedged-for window before
	// the timeout; WedgedFor is the window that declared it. Additive:
	// verdicts that settled or ran to the deadline omit both, and Reason
	// stays the cause alone.
	EarlyExit bool    `json:"earlyExit,omitempty"`
	WedgedFor string  `json:"wedgedFor,omitempty"`
	Elapsed   string  `json:"elapsed"`
	Summary   Summary `json:"summary"`
	// Fleet summarizes a fan-out run's targets by outcome.
	Fleet *FleetCounts `json:"fleet,omitempty"`
	// Contexts holds per-context rollups of a --contexts run, sorted by
	// context name. Additive: single-cluster verdicts omit it. The overall
	// status is the worst across contexts (see StatusRank), and the budget
	// layer never trims these entries — they are the skeleton of a
	// multi-cluster verdict.
	Contexts []ContextVerdict `json:"contexts,omitempty"`
	// Namespaces holds per-namespace verdicts for the namespaces with
	// non-settled targets, sorted by namespace name (by context first on
	// --contexts runs).
	Namespaces  []NamespaceVerdict `json:"namespaces,omitempty"`
	Failures []Failure `json:"failures"`
	Degraded []string  `json:"degraded"`
	// Advisories are informational notes on an otherwise-clean verdict —
	// today, fresh restart evidence on settled workloads. Structured (see
	// Advisory): automation reads the typed fields, display renders Text.
	// Omitted when empty; excluded from the content hash like Elapsed,
	// because the evidence is time-derived ("6h ago").
	Advisories []Advisory `json:"advisories,omitempty"`
	// Delta reports what moved since the --since verdict; absent without
	// the flag. Excluded from the content hash: it describes the pair of
	// runs, not the observed state.
	Delta       *Delta    `json:"delta,omitempty"`
	Truncated   Truncated `json:"truncated"`
	ContentHash string    `json:"contentHash"`
}

// StatusRank orders statuses by severity for merging: a multi-cluster
// verdict takes the worst status across its contexts, and the exit code
// follows it. An unverified cluster (permission denied, nothing matched)
// outranks progressing on purpose — "keep waiting" must never mask a cluster
// the run could not check — and everything outranks settled.
func StatusRank(s string) int {
	switch s {
	case StatusFailed:
		return 4
	case StatusPermissionDenied:
		return 3
	case StatusNoMatch:
		return 2
	case StatusProgressing:
		return 1
	}
	return 0
}

// ExitCode maps the verdict status to the process exit code. Under --since
// (Delta set, not baseline) the code reports the transition instead of the
// standing state: 3 and 4 keep their meaning — a check that broke is never
// "nothing moved" — then 0 means no transitions (a standing failure was
// already reported last run; that is the quieting delta mode exists for),
// 1/2 mean something moved and the verdict is failing/progressing, and 5
// means something moved while the verdict is settled. A baseline run (no
// previous verdict) keeps the normal codes: a loop's first run reports full
// state, not an empty diff.
func (v Verdict) ExitCode() int {
	base := ExitFailed
	switch v.Status {
	case StatusSettled:
		base = ExitSettled
	case StatusProgressing:
		base = ExitProgressing
	case StatusPermissionDenied:
		base = ExitPermission
	case StatusNoMatch:
		base = ExitNoMatch
	}
	if v.Delta == nil || v.Delta.Baseline || base == ExitPermission || base == ExitNoMatch {
		return base
	}
	if !v.Delta.Changed {
		return ExitSettled
	}
	if base == ExitSettled {
		return ExitChanged
	}
	return base
}

// Hash computes the verdict's content hash: the verdict with elapsed and the
// hash itself cleared, so two runs that observed the same state produce the
// same hash — the cheap "nothing moved" check a future delta mode needs.
// Exported so the budget layer can rehash after trimming.
func Hash(v Verdict) string {
	v.Elapsed = ""
	v.ContentHash = ""
	v.Advisories = nil
	v.Delta = nil
	b, err := json.Marshal(v)
	if err != nil {
		// A struct of plain values cannot fail to marshal.
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// WriteJSON emits the verdict as indented JSON.
func WriteJSON(w io.Writer, v Verdict) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
