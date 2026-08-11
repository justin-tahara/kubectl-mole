package signatures

import (
	"fmt"
	"regexp"
)

var admissionRe = regexp.MustCompile(`admission webhook "([^"]+)" denied the request(?::\s*(.*))?`)

// matchAdmission fires on pod creation rejected by an admission webhook,
// naming the webhook and its rejection message. Input messages come from
// ReplicaSet ReplicaFailure conditions and FailedCreate events.
func matchAdmission(msg string) *Finding {
	m := admissionRe.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	cause := fmt.Sprintf("pod creation denied by admission webhook %q", m[1])
	if m[2] != "" {
		cause += ": " + clip(m[2], maxEventEvidence)
	}
	return &Finding{
		Signature: "AdmissionRejected",
		Cause:     cause,
		Evidence:  []Evidence{{Source: "status", Text: clip(msg, maxEventEvidence)}},
	}
}
