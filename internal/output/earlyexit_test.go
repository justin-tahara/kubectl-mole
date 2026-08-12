package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// earlyExit/wedgedFor are additive: present on a wedged early exit, absent —
// not empty — everywhere else, so existing consumers and hashes see no
// change.
func TestEarlyExitFieldsAdditive(t *testing.T) {
	in := Input{
		Kind: "Deployment", Name: "api", Namespace: "prod",
		Status: StatusFailed, Reason: "container main of pod x in CrashLoopBackOff",
		EarlyExit: true, WedgedFor: 30 * time.Second, Elapsed: 31 * time.Second,
	}
	b, err := json.Marshal(Build(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"earlyExit":true`) || !strings.Contains(string(b), `"wedgedFor":"30s"`) {
		t.Fatalf("early-exit verdict missing structured fields: %s", b)
	}

	in.EarlyExit = false
	b, err = json.Marshal(Build(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "earlyExit") || strings.Contains(string(b), "wedgedFor") {
		t.Fatalf("non-wedged verdict must omit the fields entirely: %s", b)
	}
}
