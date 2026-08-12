// Package perf records optional self-measurements: wall-clock per phase and
// API request counts. Recording is off unless MOLE_METRICS_FILE names a
// destination file. The file is a side channel for the bench harness — it
// never touches the verdict, the exit code, or the output streams.
package perf

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/tools/metrics"
)

// EnvFile names the destination for the metrics JSON; unset disables all
// recording.
const EnvFile = "MOLE_METRICS_FILE"

// Metrics is the flushed schema. Phase durations accumulate under one name:
// a fan-out syncs several informer factories under a single "sync" entry.
type Metrics struct {
	SchemaVersion     int              `json:"schemaVersion"`
	PhasesMs          map[string]int64 `json:"phasesMs"`
	APIRequests       int64            `json:"apiRequests"`
	APIRequestsByVerb map[string]int64 `json:"apiRequestsByVerb"`
}

var (
	mu       sync.Mutex
	path     string
	phases   = map[string]time.Duration{}
	requests = map[string]int64{}
)

// Init enables recording when MOLE_METRICS_FILE is set. Call it before the
// first Kubernetes client is built, so the client-go hook sees every request.
func Init() {
	p := os.Getenv(EnvFile)
	if p == "" {
		return
	}
	mu.Lock()
	path = p
	mu.Unlock()
	metrics.Register(metrics.RegisterOpts{RequestResult: counter{}})
}

type counter struct{}

func (counter) Increment(_ context.Context, _ string, verb string, _ string) {
	mu.Lock()
	defer mu.Unlock()
	if path == "" {
		return
	}
	requests[verb]++
}

// Phase starts timing name and returns the stop function. Durations recorded
// under the same name accumulate.
func Phase(name string) func() {
	mu.Lock()
	enabled := path != ""
	mu.Unlock()
	if !enabled {
		return func() {}
	}
	start := time.Now()
	return func() {
		mu.Lock()
		defer mu.Unlock()
		phases[name] += time.Since(start)
	}
}

// Flush writes the metrics file. Best-effort: a bad path can never affect
// the verdict.
func Flush() {
	mu.Lock()
	defer mu.Unlock()
	if path == "" {
		return
	}
	m := Metrics{
		SchemaVersion:     1,
		PhasesMs:          make(map[string]int64, len(phases)),
		APIRequestsByVerb: make(map[string]int64, len(requests)),
	}
	for name, d := range phases {
		m.PhasesMs[name] = d.Milliseconds()
	}
	for verb, n := range requests {
		m.APIRequests += n
		m.APIRequestsByVerb[verb] = n
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}
