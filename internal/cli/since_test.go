package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// --since state handling: a missing file is the natural first run of a loop
// (baseline), a broken file is a broken loop and must fail fast, before any
// watch spends the timeout.
func TestLoadSince(t *testing.T) {
	dir := t.TempDir()

	o := &options{since: filepath.Join(dir, "missing.json")}
	if err := o.loadSince(); err != nil || o.sinceVerdict != nil {
		t.Fatalf("missing file must mean baseline, got err=%v verdict=%v", err, o.sinceVerdict)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not a verdict"), 0o644); err != nil {
		t.Fatal(err)
	}
	o = &options{since: bad}
	if err := o.loadSince(); err == nil {
		t.Fatal("a malformed state file must error")
	}

	wrong := filepath.Join(dir, "wrong.json")
	if err := os.WriteFile(wrong, []byte(`{"schemaVersion":"9"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o = &options{since: wrong}
	if err := o.loadSince(); err == nil {
		t.Fatal("a future schemaVersion must error, not silently misdiff")
	}

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"schemaVersion":"1","status":"settled","target":"Deployment/x","namespace":"app","contentHash":"sha256:x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o = &options{since: good}
	if err := o.loadSince(); err != nil || o.sinceVerdict == nil || o.sinceVerdict.Target != "Deployment/x" {
		t.Fatalf("a valid previous verdict must load, got err=%v verdict=%+v", err, o.sinceVerdict)
	}
}
