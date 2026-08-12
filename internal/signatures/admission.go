package signatures

import (
	"fmt"
	"regexp"
)

var (
	admissionWebhookRe = regexp.MustCompile(`admission webhook "([^"]+)" denied the request(?::\s*(.*))?`)
	admissionPolicyRe  = regexp.MustCompile(`ValidatingAdmissionPolicy '([^']+)' with binding '[^']+' denied request(?::\s*(.*))?`)
)

// matchAdmission fires on pod creation rejected by admission control — a
// webhook or a ValidatingAdmissionPolicy — naming the rejecting admitter and
// its message. Input messages come from ReplicaSet ReplicaFailure conditions
// and FailedCreate events.
func matchAdmission(msg string) *Finding {
	var cause string
	if m := admissionWebhookRe.FindStringSubmatch(msg); m != nil {
		cause = fmt.Sprintf("pod creation denied by admission webhook %q", m[1])
		if m[2] != "" {
			cause += ": " + clip(m[2], maxEventEvidence)
		}
	} else if m := admissionPolicyRe.FindStringSubmatch(msg); m != nil {
		cause = fmt.Sprintf("pod creation denied by validating admission policy %q", m[1])
		if m[2] != "" {
			cause += ": " + clip(m[2], maxEventEvidence)
		}
	} else {
		return nil
	}
	return &Finding{
		Signature: "AdmissionRejected",
		Cause:     cause,
		Evidence:  []Evidence{{Source: "status", Text: clip(msg, maxEventEvidence)}},
	}
}
