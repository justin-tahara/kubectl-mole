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
const (
	ExitSettled     = 0
	ExitFailed      = 1
	ExitProgressing = 2
	ExitPermission  = 3
	ExitNoMatch     = 4
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
}

// Truncated counts items dropped from the verdict. It is always emitted:
// silent truncation makes a consumer draw confident conclusions from a
// partial picture. Zeroes mean nothing was dropped. (Clipped evidence text is
// marked inline with "…(truncated)" and is not counted here.)
type Truncated struct {
	Failures int `json:"failures"`
	Evidence int `json:"evidence"`
}

// Verdict is the schemaVersion "1" output. Field names are the product:
// agents and CI pipelines bind to them.
type Verdict struct {
	SchemaVersion string    `json:"schemaVersion"`
	Status        string    `json:"status"`
	Target        string    `json:"target"`
	Namespace     string    `json:"namespace"`
	Reason        string    `json:"reason"`
	Elapsed       string    `json:"elapsed"`
	Summary       Summary   `json:"summary"`
	Failures      []Failure `json:"failures"`
	Degraded      []string  `json:"degraded"`
	Truncated     Truncated `json:"truncated"`
	ContentHash   string    `json:"contentHash"`
}

// ExitCode maps the verdict status to the process exit code.
func (v Verdict) ExitCode() int {
	switch v.Status {
	case StatusSettled:
		return ExitSettled
	case StatusProgressing:
		return ExitProgressing
	case StatusPermissionDenied:
		return ExitPermission
	case StatusNoMatch:
		return ExitNoMatch
	}
	return ExitFailed
}

// contentHash hashes the verdict with elapsed and the hash itself cleared, so
// two runs that observed the same state produce the same hash — the cheap
// "nothing moved" check a future delta mode needs.
func contentHash(v Verdict) string {
	v.Elapsed = ""
	v.ContentHash = ""
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
