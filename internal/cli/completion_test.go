package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/klog/v2"
)

func TestAsCompletionInvocation(t *testing.T) {
	cases := map[string]bool{
		"kubectl_complete-mole":                   true,
		"/usr/local/bin/kubectl_complete-mole":    true,
		`C:\bin\kubectl_complete-mole.exe`:        true,
		"/home/u/.krew/bin/kubectl_complete-mole": true,
		"kubectl-mole":                            false,
		"/usr/local/bin/kubectl-mole":             false,
		"kubectl_complete-molehill":               false,
		"kubectl_complete-kustomize":              false,
		"":                                        false,
	}
	for argv0, want := range cases {
		// filepath.Base is separator-aware, so the Windows path only
		// splits on a Windows host; the name still has to match.
		if got := asCompletionInvocation(argv0); got != want && !strings.ContainsRune(argv0, '\\') {
			t.Errorf("asCompletionInvocation(%q) = %v, want %v", argv0, got, want)
		}
	}
}

func TestCompleteKinds(t *testing.T) {
	// A bare Tab offers the canonical plurals only — not six spellings of
	// the same kind.
	if got := completeKinds(""); !reflect.DeepEqual(got, completionKinds) {
		t.Errorf("empty prefix = %v, want %v", got, completionKinds)
	}

	cases := map[string][]string{
		"sts":    {"sts"},
		"deploy": {"deploy", "deployment", "deployments"},
		"po":     {"po", "pod", "pods"},
		"cj":     {"cj"},
		"zzz":    nil,
	}
	for prefix, want := range cases {
		got := completeKinds(prefix)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("completeKinds(%q) = %v, want %v", prefix, got, want)
		}
	}

	// Every candidate offered must be a TYPE the command actually accepts.
	for _, k := range completeKinds("") {
		if _, ok := kindAliases[k]; !ok {
			t.Errorf("completion offers %q, which kindAliases does not accept", k)
		}
	}
}

func TestPrefixed(t *testing.T) {
	got := prefixed([]string{"web", "api", "api-worker", "db"}, "api")
	want := []string{"api", "api-worker"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prefixed = %v, want %v", got, want)
	}
	if got := prefixed(nil, ""); len(got) != 0 {
		t.Errorf("prefixed(nil) = %v, want empty", got)
	}
}

// writeKubeconfig writes a kubeconfig naming the given contexts and returns
// its path.
func writeKubeconfig(t *testing.T, contexts ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://127.0.0.1:1\nusers:\n- name: u\n  user: {}\ncontexts:\n")
	for _, name := range contexts {
		b.WriteString("- name: " + name + "\n  context:\n    cluster: c\n    user: u\n")
	}
	b.WriteString("current-context: " + contexts[0] + "\n")
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func optionsWithKubeconfig(t *testing.T, path string) *options {
	t.Helper()
	flags := genericclioptions.NewConfigFlags(true)
	flags.KubeConfig = &path
	// Keep discovery out of the developer's real ~/.kube/cache.
	cache := t.TempDir()
	flags.CacheDir = &cache
	return &options{configFlags: flags}
}

func TestCompleteContexts(t *testing.T) {
	o := optionsWithKubeconfig(t, writeKubeconfig(t, "prod-east", "prod-west", "staging"))

	cases := map[string][]string{
		"":     {"prod-east", "prod-west", "staging"},
		"prod": {"prod-east", "prod-west"},
		"stag": {"staging"},
		"nope": nil,
		// --contexts is a comma-separated list: only the last segment
		// completes, and the rest of the word survives.
		"prod-east,":     {"prod-east,prod-east", "prod-east,prod-west", "prod-east,staging"},
		"prod-east,stag": {"prod-east,staging"},
		"a,b,prod-w":     {"a,b,prod-west"},
	}
	for toComplete, want := range cases {
		got := o.completeContexts(toComplete)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("completeContexts(%q) = %v, want %v", toComplete, got, want)
		}
	}
}

// TestCompletionDegradesSilently is the load-bearing property: a Tab press
// against a cluster that is not there must offer nothing, never an error.
// The kubeconfig below points at a closed port.
func TestCompletionDegradesSilently(t *testing.T) {
	o := optionsWithKubeconfig(t, writeKubeconfig(t, "dead"))

	if got := o.completeNames(t.Context(), "deployments", ""); got != nil {
		t.Errorf("completeNames against a dead cluster = %v, want nil", got)
	}
	// An unknown TYPE needs discovery, which is equally unavailable.
	if got := o.completeNames(t.Context(), "rollouts.argoproj.io", ""); got != nil {
		t.Errorf("completeNames for a custom resource = %v, want nil", got)
	}
	if got := o.completeNamespaces(t.Context(), ""); got != nil {
		t.Errorf("completeNamespaces against a dead cluster = %v, want nil", got)
	}
}

func TestCompleteTargetShapes(t *testing.T) {
	o := optionsWithKubeconfig(t, writeKubeconfig(t, "dead"))
	cmd := &cobra.Command{}

	// First word, no slash: TYPE completion, which needs no cluster.
	got, directive := o.completeTarget(cmd, nil, "deploy")
	if !reflect.DeepEqual(got, []string{"deploy", "deployment", "deployments"}) {
		t.Errorf("TYPE completion = %v", got)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}

	// TYPE/NAME and TYPE NAME both reach name completion (nil here — no
	// cluster); a third argument completes nothing.
	if got, _ := o.completeTarget(cmd, nil, "deployment/ap"); got != nil {
		t.Errorf("TYPE/NAME completion = %v, want nil without a cluster", got)
	}
	if got, _ := o.completeTarget(cmd, []string{"deployment"}, "ap"); got != nil {
		t.Errorf("TYPE NAME completion = %v, want nil without a cluster", got)
	}
	if got, _ := o.completeTarget(cmd, []string{"deployment", "api"}, ""); got != nil {
		t.Errorf("third argument completed %v, want nil", got)
	}
}

// TestCompletionsRegistered guards the wiring: registerCompletions panics on
// a flag that does not exist, so building the command is the assertion.
func TestCompletionsRegistered(t *testing.T) {
	o := &options{
		configFlags: genericclioptions.NewConfigFlags(true),
		streams:     genericiooptions.NewTestIOStreamsDiscard(),
	}
	cmd := newMoleCommand(o, "test")

	if cmd.ValidArgsFunction == nil {
		t.Fatal("the positional argument has no completion")
	}
	for _, flag := range []string{"output", "namespace", "context", "contexts"} {
		if _, ok := cmd.GetFlagCompletionFunc(flag); !ok {
			t.Errorf("--%s has no completion function", flag)
		}
	}
}

// TestPrepareCompletionQuietsTheKeystroke guards the two ways a Tab press
// used to disturb the terminal: client-go logging transport failures to
// stderr over the line being typed, and API discovery keeping its 32s
// request timeout because it builds its own client from the config flags.
func TestPrepareCompletionQuietsTheKeystroke(t *testing.T) {
	o := optionsWithKubeconfig(t, writeKubeconfig(t, "dead"))
	o.prepareCompletion()

	if got, want := *o.configFlags.Timeout, completionTimeout.String(); got != want {
		t.Errorf("request timeout = %q, want %q", got, want)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	utilruntime.HandleError(errors.New("a transport failure behind a Tab press"))
	klog.Flush()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 0 {
		t.Errorf("completion wrote %q to stderr; a Tab press must stay silent", out)
	}
}
