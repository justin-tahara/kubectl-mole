package cli

import (
	"fmt"
	"testing"

	"github.com/justin-tahara/kubectl-mole/internal/output"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

func TestErrorVerdictMapping(t *testing.T) {
	target := settle.Target{Kind: settle.KindDeployment, Namespace: "prod", Name: "api"}

	v, ok := errorVerdict(&settle.NotFoundError{Target: target}, "Deployment", "api", "prod")
	if !ok || v.Status != output.StatusNoMatch || v.ExitCode() != output.ExitNoMatch {
		t.Fatalf("not-found: ok=%v status=%q exit=%d", ok, v.Status, v.ExitCode())
	}

	v, ok = errorVerdict(&settle.PermissionError{Verb: "list", Resource: "pods", Namespace: "prod"}, "Deployment", "api", "prod")
	if !ok || v.Status != output.StatusPermissionDenied || v.ExitCode() != output.ExitPermission {
		t.Fatalf("permission: ok=%v status=%q exit=%d", ok, v.Status, v.ExitCode())
	}
	if v.Reason != "cannot list pods in namespace prod" {
		t.Fatalf("reason %q must name verb and resource", v.Reason)
	}

	// A wrapped typed error still maps.
	wrapped := fmt.Errorf("watch: %w", &settle.NotFoundError{Target: target})
	if _, ok := errorVerdict(wrapped, "Deployment", "api", "prod"); !ok {
		t.Fatal("wrapped NotFoundError must still map to a verdict")
	}

	// Anything else stays an error.
	if _, ok := errorVerdict(fmt.Errorf("boom"), "Deployment", "api", "prod"); ok {
		t.Fatal("generic errors must not become verdicts")
	}
}

func TestStatusFor(t *testing.T) {
	cases := map[settle.Outcome]string{
		settle.OutcomeSettled:     output.StatusSettled,
		settle.OutcomeProgressing: output.StatusProgressing,
		settle.OutcomeFailed:      output.StatusFailed,
	}
	for outcome, want := range cases {
		if got := statusFor(outcome); got != want {
			t.Errorf("statusFor(%s) = %q, want %q", outcome, got, want)
		}
	}
}
