package cli

import (
	"context"
	"flag"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
)

// This file is shell completion. kubectl (v1.26+) completes a plugin's
// arguments by running `kubectl_complete-<plugin>` from PATH and reading
// cobra's __complete output from it. The mole binary answers to that name
// itself — invoked as kubectl_complete-mole it prepends the hidden
// __complete verb — so completion installs as one symlink and never drifts
// from the binary's real flag surface.
//
// Nothing here may fail loudly. A Tab press that prints a diagnostic over
// the command line is worse than a Tab press that offers nothing, so every
// cluster read degrades to no completions.

// completionName is the executable name kubectl looks for on PATH.
const completionName = "kubectl_complete-mole"

// completionTimeout bounds the cluster reads behind a Tab press.
const completionTimeout = 3 * time.Second

// completionLimit caps a completion listing. Past a few hundred names the
// menu is unusable anyway, and the point is to keep one keystroke cheap.
const completionLimit = 500

// completionKinds is what a bare Tab offers: the canonical plural of each
// built-in kind, kubectl's own habit. The short aliases still complete —
// they join the list once a typed prefix matches one, so `sts<TAB>` works
// without an empty menu listing six spellings of Deployment.
var completionKinds = []string{
	"cronjobs", "daemonsets", "deployments", "jobs", "pods", "statefulsets",
}

// completionGVRs resolves the built-in kinds without API discovery; an
// unknown TYPE goes through the REST mapper like a real run would.
var completionGVRs = map[settle.Kind]schema.GroupVersionResource{
	settle.KindDeployment:  {Group: "apps", Version: "v1", Resource: "deployments"},
	settle.KindStatefulSet: {Group: "apps", Version: "v1", Resource: "statefulsets"},
	settle.KindDaemonSet:   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	settle.KindJob:         {Group: "batch", Version: "v1", Resource: "jobs"},
	settle.KindCronJob:     {Group: "batch", Version: "v1", Resource: "cronjobs"},
	settle.KindPod:         {Group: "", Version: "v1", Resource: "pods"},
}

var namespaceGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

// asCompletionInvocation reports whether argv0 names the completion
// executable rather than the plugin itself. A symlink, a hard link and a
// copy all work; only the name decides.
func asCompletionInvocation(argv0 string) bool {
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	return base == completionName
}

// prepareCompletion makes the process safe to run behind a keystroke.
//
// Two things leak past the per-read timeout otherwise. client-go logs
// transport failures through klog straight to stderr, which paints an error
// across the command line the user is still typing; and API discovery — the
// path an unknown TYPE takes — is built from the config flags rather than
// the config completion reads use, so it keeps the 32s default request
// timeout. Both are fine for a real run and wrong for a Tab press.
func (o *options) prepareCompletion() {
	// klog writes error-severity records to stderr whatever its output
	// writer is set to — stderrThreshold defaults to ERROR — so the
	// threshold has to move too.
	var fs flag.FlagSet
	klog.InitFlags(&fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
	klog.SetOutput(io.Discard)
	if o.configFlags.Timeout != nil {
		// A default, not an override: --request-timeout is parsed after
		// this and still wins.
		*o.configFlags.Timeout = completionTimeout.String()
	}
}

// registerCompletions wires the completion functions onto the command. A
// flag named here that does not exist is a wiring bug, not a runtime
// condition, so it panics rather than silently completing nothing.
func (o *options) registerCompletions(cmd *cobra.Command) {
	cmd.ValidArgsFunction = o.completeTarget

	register := func(name string, fn cobra.CompletionFunc) {
		if err := cmd.RegisterFlagCompletionFunc(name, fn); err != nil {
			panic(err)
		}
	}
	register("output", cobra.FixedCompletions([]string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp))
	register("namespace", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return o.completeNamespaces(cmd.Context(), toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	for _, name := range []string{"context", "contexts"} {
		register(name, func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return o.completeContexts(toComplete), cobra.ShellCompDirectiveNoFileComp
		})
	}
}

// completeTarget completes the positional argument, in either spelling mole
// accepts: TYPE/NAME as one word, or TYPE and NAME as two.
func (o *options) completeTarget(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch {
	// TYPE/NAME — complete the name, echoing the type back so the shell
	// replaces the whole word.
	case len(args) == 0 && strings.Contains(toComplete, "/"):
		typeArg, prefix, _ := strings.Cut(toComplete, "/")
		var out []string
		for _, n := range o.completeNames(cmd.Context(), typeArg, prefix) {
			out = append(out, typeArg+"/"+n)
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	case len(args) == 0:
		return completeKinds(toComplete), cobra.ShellCompDirectiveNoFileComp
	// TYPE NAME — the type is already its own argument.
	case len(args) == 1 && !strings.Contains(args[0], "/"):
		return o.completeNames(cmd.Context(), args[0], toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeKinds offers the TYPE argument, adding the short aliases only
// once a prefix narrows them.
func completeKinds(prefix string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] && strings.HasPrefix(s, prefix) {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, k := range completionKinds {
		add(k)
	}
	if prefix != "" {
		for alias := range kindAliases {
			add(alias)
		}
	}
	sort.Strings(out)
	return out
}

// completeNames lists the resource names of one TYPE in the namespace the
// flags resolve to.
func (o *options) completeNames(ctx context.Context, typeArg, prefix string) []string {
	ctx, cfg, cancel, ok := o.completionConfig(ctx)
	if !ok {
		return nil
	}
	defer cancel()

	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil
	}
	gvr, ok := completionGVRs[kindAliases[strings.ToLower(typeArg)]]
	if !ok {
		mapper, err := o.configFlags.ToRESTMapper()
		if err != nil {
			return nil
		}
		if gvr, _, err = resolveType(mapper, typeArg); err != nil {
			return nil
		}
	}
	return prefixed(listNames(ctx, cfg, gvr, ns), prefix)
}

// completeNamespaces lists namespaces for -n. It is a cluster-scoped read,
// so it needs no namespace of its own.
func (o *options) completeNamespaces(ctx context.Context, prefix string) []string {
	ctx, cfg, cancel, ok := o.completionConfig(ctx)
	if !ok {
		return nil
	}
	defer cancel()
	return prefixed(listNames(ctx, cfg, namespaceGVR, ""), prefix)
}

// completeContexts completes kubeconfig context names for --context and
// --contexts. --contexts takes a comma-separated list, so only the segment
// after the last comma is completed and everything before it is carried
// through — the shell replaces the whole word either way.
func (o *options) completeContexts(toComplete string) []string {
	raw, err := o.configFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil
	}
	head, prefix := "", toComplete
	if i := strings.LastIndex(toComplete, ","); i >= 0 {
		head, prefix = toComplete[:i+1], toComplete[i+1:]
	}
	var out []string
	for name := range raw.Contexts {
		if strings.HasPrefix(name, prefix) {
			out = append(out, head+name)
		}
	}
	sort.Strings(out)
	return out
}

// completionConfig builds the REST config for a completion read and the
// context that bounds it. The caller must call cancel when ok.
func (o *options) completionConfig(ctx context.Context) (context.Context, *rest.Config, context.CancelFunc, bool) {
	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return nil, nil, nil, false
	}
	cfg.QPS, cfg.Burst = o.qps, o.burst
	cfg.Timeout = completionTimeout
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	return ctx, cfg, cancel, true
}

// listNames lists one resource's names, returning nothing on any failure:
// no cluster, no permission, no such type.
func listNames(ctx context.Context, cfg *rest.Config, gvr schema.GroupVersionResource, ns string) []string {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil
	}
	list, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{Limit: completionLimit})
	if err != nil {
		return nil
	}
	var names []string
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	return names
}

// prefixed keeps the candidates the typed prefix matches, sorted. The shell
// filters too, but a plugin that returns the whole namespace on every
// keystroke wastes the round trip it just paid for.
func prefixed(names []string, prefix string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
