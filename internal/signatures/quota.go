package signatures

import (
	"fmt"
	"regexp"
	"strings"
)

var quotaRe = regexp.MustCompile(`exceeded quota:\s*([^,]+), requested:\s*([^,]+), used:`)

// matchQuota fires on pod creation blocked by a ResourceQuota, naming the
// quota and the requested resource. Input messages come from ReplicaSet
// ReplicaFailure conditions and FailedCreate events.
func matchQuota(msg string) *Finding {
	if !strings.Contains(msg, "exceeded quota") {
		return nil
	}
	cause := "pod creation blocked by resource quota"
	if m := quotaRe.FindStringSubmatch(msg); m != nil {
		cause = fmt.Sprintf("pod creation blocked by quota %q (requested %s)", strings.TrimSpace(m[1]), strings.TrimSpace(m[2]))
	}
	return &Finding{
		Signature: "QuotaExceeded",
		Cause:     cause,
		Evidence:  []Evidence{{Source: "status", Text: clip(msg, maxEventEvidence)}},
	}
}
