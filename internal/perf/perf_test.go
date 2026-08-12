package perf

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// One test drives the disabled and enabled states in order: the recorder is
// process-global, so the disabled assertions must run before Init.
func TestRecorder(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "metrics.json")

	// Disabled: no env, no file, hooks are no-ops.
	Phase("preflight")()
	counter{}.Increment(context.Background(), "200", "GET", "host")
	Flush()
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("disabled recorder must not write: stat err = %v", err)
	}

	t.Setenv(EnvFile, dest)
	Init()
	stop := Phase("watch")
	time.Sleep(2 * time.Millisecond)
	stop()
	counter{}.Increment(context.Background(), "200", "GET", "host")
	counter{}.Increment(context.Background(), "200", "GET", "host")
	counter{}.Increment(context.Background(), "201", "POST", "host")
	Flush()

	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read metrics file: %v", err)
	}
	var m Metrics
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", m.SchemaVersion)
	}
	if _, ok := m.PhasesMs["watch"]; !ok {
		t.Errorf("phasesMs missing %q: %v", "watch", m.PhasesMs)
	}
	if _, ok := m.PhasesMs["preflight"]; ok {
		t.Errorf("disabled-phase %q must not be recorded: %v", "preflight", m.PhasesMs)
	}
	if m.APIRequests != 3 {
		t.Errorf("apiRequests = %d, want 3", m.APIRequests)
	}
	if m.APIRequestsByVerb["GET"] != 2 || m.APIRequestsByVerb["POST"] != 1 {
		t.Errorf("apiRequestsByVerb = %v", m.APIRequestsByVerb)
	}
}
