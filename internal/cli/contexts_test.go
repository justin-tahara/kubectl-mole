package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/justin-tahara/kubectl-mole/internal/output"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: alpha
  context: {cluster: c1, user: u1}
- name: beta
  context: {cluster: c1, user: u1, namespace: nsb}
users:
- name: u1
  user: {}
current-context: alpha
`

func contextOptions(t *testing.T, contexts []string) *options {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &options{configFlags: genericclioptions.NewConfigFlags(true), contexts: contexts}
	o.configFlags.KubeConfig = &path
	return o
}

func TestContextNamesSortedAndDeduped(t *testing.T) {
	o := contextOptions(t, []string{"beta", "alpha", "beta", ""})
	names, err := o.contextNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("want [alpha beta], got %v", names)
	}
}

func TestContextNamesUnknownContextFailsFast(t *testing.T) {
	o := contextOptions(t, []string{"alpha", "gamma"})
	if _, err := o.contextNames(); err == nil || !strings.Contains(err.Error(), "gamma") {
		t.Fatalf("unknown context must fail naming it, got %v", err)
	}
}

func TestContextNamesRejectsContextFlag(t *testing.T) {
	o := contextOptions(t, []string{"alpha"})
	single := "beta"
	o.configFlags.Context = &single
	if _, err := o.contextNames(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("--context beside --contexts must error, got %v", err)
	}
}

func TestContextErrorEntryMapping(t *testing.T) {
	cases := []struct {
		err    error
		status string
	}{
		{&settle.NotFoundError{Target: settle.Target{Kind: settle.KindDeployment, Namespace: "prod", Name: "api"}}, output.StatusNoMatch},
		{&settle.NoMatchError{Selector: "a=b", Namespace: "prod"}, output.StatusNoMatch},
		{&settle.PermissionError{Verb: "list", Resource: "pods", Namespace: "prod"}, output.StatusPermissionDenied},
		{errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), output.StatusFailed},
	}
	for _, c := range cases {
		e := contextErrorEntry(c.err)
		if e.Status != c.status {
			t.Fatalf("%v: got status %q, want %q", c.err, e.Status, c.status)
		}
		if e.Reason == "" {
			t.Fatalf("%v: entry lost its reason", c.err)
		}
	}
}

func TestMergedNamespace(t *testing.T) {
	o := contextOptions(t, nil)

	flagNS := "forced"
	o.configFlags.Namespace = &flagNS
	if got := o.mergedNamespace(nil); got != "forced" {
		t.Fatalf("-n must win, got %q", got)
	}

	empty := ""
	o.configFlags.Namespace = &empty
	agree := []contextOutcome{{ns: "default"}, {ns: "default"}}
	if got := o.mergedNamespace(agree); got != "default" {
		t.Fatalf("agreeing defaults must surface, got %q", got)
	}
	disagree := []contextOutcome{{ns: "default"}, {ns: "nsb"}}
	if got := o.mergedNamespace(disagree); got != "" {
		t.Fatalf("disagreeing defaults must render as *, got %q", got)
	}

	o.allNamespaces = true
	if got := o.mergedNamespace(agree); got != "" {
		t.Fatalf("-A must render as *, got %q", got)
	}
}
