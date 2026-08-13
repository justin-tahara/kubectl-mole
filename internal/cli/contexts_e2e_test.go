//go:build e2e

// End-to-end tests for the --contexts multi-cluster passthrough. Multi-cluster
// SEMANTICS (concurrent fan-out, per-context rollup, cross-context collapse,
// a dead cluster failing the verdict) need two contexts, not two clusters —
// so the setup writes a kubeconfig aliasing the one disposable cluster under
// two context names, plus a third context pointing at a dead endpoint.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"

	"github.com/justin-tahara/kubectl-mole/internal/output"
)

const e2eImage = "public.ecr.aws/docker/library/busybox:1.36"

// e2eSetup returns a client for staging resources and the path of the
// aliased kubeconfig carrying contexts mole-east, mole-west (the live
// cluster) and mole-dead (nothing listens there).
func e2eSetup(t *testing.T) (*kubernetes.Clientset, string) {
	t.Helper()
	ctxName := os.Getenv("MOLE_E2E_CONTEXT")
	if ctxName == "" {
		t.Skip("MOLE_E2E_CONTEXT not set; set it to a disposable cluster's context (e.g. kind-mole-dev) to run e2e tests")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := rules.Load()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	src, ok := raw.Contexts[ctxName]
	if !ok {
		t.Fatalf("context %q not in kubeconfig", ctxName)
	}
	cluster, ok := raw.Clusters[src.Cluster]
	if !ok {
		t.Fatalf("cluster %q not in kubeconfig", src.Cluster)
	}
	user, ok := raw.AuthInfos[src.AuthInfo]
	if !ok {
		t.Fatalf("user %q not in kubeconfig", src.AuthInfo)
	}

	aliased := clientcmdapi.NewConfig()
	aliased.Clusters["live"] = cluster
	aliased.AuthInfos["u"] = user
	for _, name := range []string{"mole-east", "mole-west"} {
		aliased.Contexts[name] = &clientcmdapi.Context{Cluster: "live", AuthInfo: "u", Namespace: src.Namespace}
	}
	dead := clientcmdapi.NewCluster()
	dead.Server = "https://127.0.0.1:1"
	dead.InsecureSkipTLSVerify = true
	aliased.Clusters["dead"] = dead
	aliased.Contexts["mole-dead"] = &clientcmdapi.Context{Cluster: "dead", AuthInfo: "u"}
	aliased.CurrentContext = "mole-east"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*aliased, path); err != nil {
		t.Fatalf("write aliased kubeconfig: %v", err)
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: ctxName}).ClientConfig()
	if err != nil {
		t.Fatalf("load client config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return cs, path
}

func e2eNamespace(t *testing.T, cs *kubernetes.Clientset) string {
	t.Helper()
	ns, err := cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mole-cli-e2e-"}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return ns.Name
}

func e2eDeployment(t *testing.T, cs *kubernetes.Clientset, ns, name string, mut func(*appsv1.Deployment)) {
	t.Helper()
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		// Labels on the workload itself too: fleet discovery selects on
		// workload labels, not the pod template's.
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(2)),
					Containers: []corev1.Container{{
						Name:    "main",
						Image:   e2eImage,
						Command: []string{"sh", "-c", "sleep 3600"},
					}},
				},
			},
		},
	}
	if mut != nil {
		mut(d)
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
}

// runMole drives the real command — flag parsing, dispatch, emit — and
// returns the parsed verdict plus the exit code the process would return.
func runMole(t *testing.T, ctx context.Context, args ...string) (output.Verdict, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := &options{
		configFlags: genericclioptions.NewConfigFlags(true),
		streams:     genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errOut},
	}
	cmd := newMoleCommand(o, "e2e")
	cmd.SetArgs(args)
	cmd.SetOut(&errOut)
	cmd.SetErr(&errOut)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("mole %v errored: %v\n%s", args, err, errOut.String())
	}
	var v output.Verdict
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("verdict is not JSON: %v\n%s", err, out.String())
	}
	t.Logf("mole %v -> %s exit %d (%s)", args, v.Status, o.exitCode, v.Reason)
	return v, o.exitCode
}

// A healthy workload seen through two contexts: one settled verdict, exit 0,
// a rollup entry per context, and the named target kept.
func TestContextsSettleAcrossContexts(t *testing.T) {
	t.Parallel()
	cs, kubeconfig := e2eSetup(t)
	ns := e2eNamespace(t, cs)
	e2eDeployment(t, cs, ns, "steady", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// Glob patterns on purpose: this also proves pattern expansion through
	// the real CLI (mole-e* and mole-w* resolve to east/west, never dead).
	v, exit := runMole(t, ctx, "deployment/steady", "-n", ns,
		"--contexts", "mole-e*,mole-w*", "--kubeconfig", kubeconfig,
		"-o", "json", "--timeout", "2m", "--stable-for", "5s")

	if v.Status != output.StatusSettled || exit != 0 {
		t.Fatalf("want settled/0, got %s/%d (%s)", v.Status, exit, v.Reason)
	}
	if v.Target != "Deployment/steady" {
		t.Fatalf("named multi-context target lost: %q", v.Target)
	}
	if len(v.Contexts) != 2 || v.Contexts[0].Context != "mole-east" || v.Contexts[1].Context != "mole-west" {
		t.Fatalf("rollup wrong: %+v", v.Contexts)
	}
	for _, c := range v.Contexts {
		if c.Status != output.StatusSettled {
			t.Fatalf("context %s not settled: %+v", c.Context, c)
		}
	}
	if v.Fleet == nil || v.Fleet.Contexts != 2 || v.Fleet.Targets != 2 || v.Fleet.Settled != 2 {
		t.Fatalf("fleet counts wrong: %+v", v.Fleet)
	}
}

// The collapse proof: one crash-looping workload seen through two contexts
// must fold into ONE failure entry whose anchors span both contexts. A naive
// per-context concatenation would emit two entries and fail here.
func TestContextsCollapseAcrossContexts(t *testing.T) {
	t.Parallel()
	cs, kubeconfig := e2eSetup(t)
	ns := e2eNamespace(t, cs)
	e2eDeployment(t, cs, ns, "crash", func(d *appsv1.Deployment) {
		d.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "exit 7"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	// --stable-for stays at the default-strength 15s on purpose: a crash-loop
	// pod without a readiness probe flickers Ready for the instant before it
	// exits, and under CI load one context's informer cache can hold that
	// stale-healthy deployment status past a short window — settling a
	// context this test needs to fail.
	v, exit := runMole(t, ctx, "-n", ns, "-l", "app=crash",
		"--contexts", "mole-east,mole-west", "--kubeconfig", kubeconfig,
		"-o", "json", "--timeout", "4m", "--stable-for", "15s", "--wedged-for", "20s")

	if v.Status != output.StatusFailed || exit != 1 {
		t.Fatalf("want failed/1, got %s/%d (%s)", v.Status, exit, v.Reason)
	}
	for _, c := range v.Contexts {
		if c.Status != output.StatusFailed {
			t.Fatalf("context %s must fail, got %q (%s); contexts: %+v", c.Context, c.Status, c.Reason, v.Contexts)
		}
	}
	if len(v.Failures) != 1 {
		t.Fatalf("one cause via two contexts must collapse to 1 entry, got %d: %+v", len(v.Failures), v.Failures)
	}
	f := v.Failures[0]
	if f.Affected < 2 {
		t.Fatalf("anchors must span both contexts, affected=%d; failures: %+v contexts: %+v", f.Affected, v.Failures, v.Contexts)
	}
	for _, prefix := range []string{"mole-east/", "mole-west/"} {
		found := false
		for _, ex := range f.Examples {
			if strings.HasPrefix(ex, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("examples must carry both contexts, got %v", f.Examples)
		}
	}
}

// A cluster that cannot be reached must fail the verdict and be named in the
// rollup — and must not stop the live context from reporting.
func TestContextsDeadClusterReported(t *testing.T) {
	t.Parallel()
	cs, kubeconfig := e2eSetup(t)
	ns := e2eNamespace(t, cs)
	e2eDeployment(t, cs, ns, "survivor", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	v, exit := runMole(t, ctx, "deployment/survivor", "-n", ns,
		"--contexts", "mole-east,mole-dead", "--kubeconfig", kubeconfig,
		"-o", "json", "--timeout", "2m", "--stable-for", "5s")

	if v.Status != output.StatusFailed || exit != 1 {
		t.Fatalf("an unreachable cluster must fail the verdict, got %s/%d (%s)", v.Status, exit, v.Reason)
	}
	byName := map[string]output.ContextVerdict{}
	for _, c := range v.Contexts {
		byName[c.Context] = c
	}
	if byName["mole-east"].Status != output.StatusSettled {
		t.Fatalf("live context must still settle: %+v", v.Contexts)
	}
	if byName["mole-dead"].Status != output.StatusFailed {
		t.Fatalf("dead context must report failed: %+v", v.Contexts)
	}
}
