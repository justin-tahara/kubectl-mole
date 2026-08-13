// Package cli wires the kubectl-mole command. Kubeconfig handling comes from
// k8s.io/cli-runtime so --context, --namespace, --kubeconfig behave exactly
// like kubectl.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/justin-tahara/kubectl-mole/internal/budget"
	"github.com/justin-tahara/kubectl-mole/internal/collapse"
	"github.com/justin-tahara/kubectl-mole/internal/output"
	"github.com/justin-tahara/kubectl-mole/internal/perf"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

var kindAliases = map[string]settle.Kind{
	"deployment": settle.KindDeployment, "deployments": settle.KindDeployment, "deploy": settle.KindDeployment,
	"statefulset": settle.KindStatefulSet, "statefulsets": settle.KindStatefulSet, "sts": settle.KindStatefulSet,
	"daemonset": settle.KindDaemonSet, "daemonsets": settle.KindDaemonSet, "ds": settle.KindDaemonSet,
	"job": settle.KindJob, "jobs": settle.KindJob,
	"cronjob": settle.KindCronJob, "cronjobs": settle.KindCronJob, "cj": settle.KindCronJob,
	"pod": settle.KindPod, "pods": settle.KindPod, "po": settle.KindPod,
}

type options struct {
	configFlags   *genericclioptions.ConfigFlags
	output        string
	timeout       time.Duration
	stableFor     time.Duration
	wedgedFor     time.Duration
	restartWindow time.Duration
	budget        int
	selector      string
	since         string
	// sinceVerdict is the parsed --since file; nil with since set means
	// the file did not exist — a baseline run.
	sinceVerdict *output.Verdict
	contexts     []string
	allNamespaces bool
	maxTargets    int
	includeJobs   bool
	noColor       bool
	qps           float32
	burst         int
	streams       genericiooptions.IOStreams

	// exitCode is the verdict's exit code: 0 settled, 1 failed, 2 timed out
	// while still progressing, 3 permission denied, 4 no resources matched.
	exitCode int
}

// Execute runs the command and returns the process exit code.
func Execute(streams genericiooptions.IOStreams, version string) int {
	o := &options{
		configFlags: genericclioptions.NewConfigFlags(true),
		streams:     streams,
	}
	cmd := newMoleCommand(o, version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.ExecuteContext(ctx); err != nil {
		return 1
	}
	return o.exitCode
}

func newMoleCommand(o *options, version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "kubectl-mole [TYPE/NAME]",
		Short:   "Watch resources until they settle, then explain what broke",
		Long:    "kubectl-mole watches Kubernetes resources until they settle, then emits one structured verdict explaining what happened and, if something failed, why.\n\nDeployments, StatefulSets and DaemonSets settle by holding healthy; Jobs, CronJobs and bare Pods settle by completing. With no TYPE/NAME argument it fans out over every Deployment, StatefulSet and DaemonSet in scope (Jobs too with --include-jobs) — one namespace, or all of them with --all-namespaces, optionally filtered by --selector.",
		Example: "  kubectl mole deployment/api -n prod\n  kubectl mole sts/db --timeout 3m --stable-for 20s -o json\n  kubectl mole job/migrate -n prod\n  kubectl mole -n prod -l app.kubernetes.io/name=api\n  kubectl mole --all-namespaces -l app.kubernetes.io/part-of=platform -o json --budget 800\n  kubectl mole deployment/api -n prod --contexts us-east,us-west\n  kubectl mole --contexts us-east,us-west -A -l app.kubernetes.io/instance=my-release",
		Version: version,
		Args:    cobra.RangeArgs(0, 2),
		// The command handles its own errors and exit codes; cobra should not
		// print usage after a runtime failure.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), args)
		},
	}

	// `kubectl mole version` — the kubectl habit. Same line --version
	// prints; no workload TYPE is plausibly named "version", and cobra only
	// routes the bare word here.
	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the kubectl-mole version",
		Args:  cobra.NoArgs,
		Run: func(c *cobra.Command, _ []string) {
			fmt.Fprintf(o.streams.Out, "kubectl-mole version %s\n", version)
		},
	})
	// Subcommands make cobra offer its completion machinery; kubectl owns
	// plugin completion, so keep the surface at exactly one verb plus
	// version.
	cmd.CompletionOptions.DisableDefaultCmd = true

	cmd.Flags().StringVarP(&o.output, "output", "o", "text", "output format: text or json")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 2*time.Minute, "max wall-clock time to wait for settle")
	cmd.Flags().DurationVar(&o.stableFor, "stable-for", 15*time.Second, "how long a healthy state must hold before it counts as settled")
	cmd.Flags().DurationVar(&o.restartWindow, "restart-window", 24*time.Hour, "annotate settled verdicts when containers terminated within this window (0 = no advisory)")
	cmd.Flags().DurationVar(&o.wedgedFor, "wedged-for", 30*time.Second, "declare failure once a pod has spent this long wedged in a terminal-failure state, instead of waiting out the timeout (0 = only fail at timeout)")
	cmd.Flags().IntVar(&o.budget, "budget", 0, "approximate token budget for output; 0 = unlimited (advisory, ~3 chars/token)")
	cmd.Flags().StringVar(&o.since, "since", "", "path to a previous -o json verdict: report what changed since it and exit 0 when nothing did (missing file = baseline run with normal exit codes)")
	cmd.Flags().StringVarP(&o.selector, "selector", "l", "", "label selector for fan-out over workloads (instead of TYPE/NAME)")
	cmd.Flags().StringSliceVar(&o.contexts, "contexts", nil, "kubeconfig contexts to check concurrently (comma-separated or repeated); all clusters merge into one verdict")
	cmd.Flags().BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "fan out across all namespaces")
	cmd.Flags().IntVar(&o.maxTargets, "max-targets", settle.DefaultMaxTargets, "refuse a fan-out matching more workloads than this")
	cmd.Flags().BoolVar(&o.includeJobs, "include-jobs", false, "add Jobs to the fan-out (off by default: batch churn drowns fleet verdicts)")
	cmd.Flags().BoolVar(&o.noColor, "no-color", false, "disable styled terminal output (piped output is always plain)")
	cmd.Flags().Float32Var(&o.qps, "qps", 20, "client-side API request rate (queries per second)")
	cmd.Flags().IntVar(&o.burst, "burst", 30, "client-side API request burst allowance")
	o.configFlags.AddFlags(cmd.Flags())
	return cmd
}

// splitTypeName extracts the TYPE and NAME from the positional arguments.
func splitTypeName(args []string) (string, string, error) {
	switch len(args) {
	case 1:
		parts := strings.SplitN(args[0], "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("expected TYPE/NAME (e.g. deployment/api), got %q", args[0])
		}
		return parts[0], parts[1], nil
	default:
		return args[0], args[1], nil
	}
}

// resolveType maps a TYPE the alias table does not know — usually a custom
// resource — through API discovery, kubectl-style: plural, singular, or
// shortname, with an optional .group suffix (rollouts.argoproj.io).
func resolveType(mapper meta.RESTMapper, arg string) (schema.GroupVersionResource, schema.GroupVersionKind, error) {
	res := strings.ToLower(arg)
	group := ""
	if i := strings.Index(res, "."); i >= 0 {
		res, group = res[:i], res[i+1:]
	}
	gvr, err := mapper.ResourceFor(schema.GroupVersionResource{Group: group, Resource: res})
	if err != nil {
		return schema.GroupVersionResource{}, schema.GroupVersionKind{}, fmt.Errorf("unknown resource type %q: %w", arg, err)
	}
	gvk, err := mapper.KindFor(gvr)
	if err != nil {
		return schema.GroupVersionResource{}, schema.GroupVersionKind{}, fmt.Errorf("resolve kind for %q: %w", arg, err)
	}
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, schema.GroupVersionKind{}, err
	}
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		return schema.GroupVersionResource{}, schema.GroupVersionKind{}, fmt.Errorf("%s is cluster-scoped; mole watches namespaced resources", gvr.Resource)
	}
	return gvr, gvk, nil
}

func (o *options) run(ctx context.Context, args []string) error {
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", o.output)
	}
	if err := validateSelection(args, o.selector, o.allNamespaces); err != nil {
		return err
	}
	if err := o.loadSince(); err != nil {
		return err
	}
	if len(o.contexts) > 0 {
		return o.runContexts(ctx, args)
	}

	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("resolve namespace: %w", err)
	}
	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.QPS = o.qps
	cfg.Burst = o.burst
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	if len(args) == 0 {
		if o.allNamespaces {
			ns = ""
		}
		return o.runFleet(ctx, cs, ns)
	}

	typeArg, name, err := splitTypeName(args)
	if err != nil {
		return err
	}
	kind, ok := kindAliases[strings.ToLower(typeArg)]
	if !ok {
		return o.runCustom(ctx, cs, cfg, typeArg, name, ns)
	}
	target := settle.Target{Kind: kind, Namespace: ns, Name: name}
	draw, clearLine := o.digStatus(target.String())
	res, err := settle.Run(ctx, cs, target, settle.Options{Timeout: o.timeout, StableFor: o.stableFor, WedgedFor: o.wedgedFor, Progress: draw})
	clearLine()
	if err != nil {
		if v, ok := errorVerdict(err, string(kind), name, ns); ok {
			return o.emit(v)
		}
		return err
	}

	var rep signatures.Report
	if res.Outcome != settle.OutcomeSettled {
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		defer dcancel()
		rep = signatures.Diagnose(dctx, cs, signatures.TargetRef{Kind: string(kind), Namespace: ns, Name: name}, res.Final.CurrentPods, res.Final.OldPods)
	}

	return o.emit(output.Build(output.Input{
		Kind:       string(kind),
		Name:       name,
		Namespace:  ns,
		Status:     statusFor(res.Outcome),
		Reason:     res.Reason,
		EarlyExit:  res.WedgedOut,
		WedgedFor:  o.wedgedFor,
		Elapsed:    res.Elapsed,
		Pods:       res.Final.CurrentPods,
		OldPods:    res.Final.OldPods,
		Advisories: o.settledAdvisories(ctx, cs, res),
		Failures:   collapse.Collapse(rep.Findings),
		Degraded:   rep.Degraded,
	}))
}

// runCustom watches a resource outside the alias table — usually a custom
// resource — through the dynamic engine, after resolving the type via API
// discovery.
func (o *options) runCustom(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, typeArg, name, ns string) error {
	mapper, err := o.configFlags.ToRESTMapper()
	if err != nil {
		return fmt.Errorf("build REST mapper: %w", err)
	}
	gvr, gvk, err := resolveType(mapper, typeArg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}

	draw, clearLine := o.digStatus(gvk.Kind + "/" + name)
	res, err := settle.RunCustom(ctx, cs, dyn, gvr, gvk.Kind, ns, name, settle.Options{Timeout: o.timeout, StableFor: o.stableFor, WedgedFor: o.wedgedFor, Progress: draw})
	clearLine()
	if err != nil {
		if v, ok := errorVerdict(err, gvk.Kind, name, ns); ok {
			return o.emit(v)
		}
		return err
	}

	var rep signatures.Report
	if res.Outcome != settle.OutcomeSettled {
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		defer dcancel()
		rep = signatures.Diagnose(dctx, cs, signatures.TargetRef{Kind: gvk.Kind, Namespace: ns, Name: name}, res.Final.CurrentPods, res.Final.OldPods)
	}

	return o.emit(output.Build(output.Input{
		Kind:       gvk.Kind,
		Name:       name,
		Namespace:  ns,
		Status:     statusFor(res.Outcome),
		Reason:     res.Reason,
		EarlyExit:  res.WedgedOut,
		WedgedFor:  o.wedgedFor,
		Elapsed:    res.Elapsed,
		Pods:       res.Final.CurrentPods,
		OldPods:    res.Final.OldPods,
		Advisories: o.settledAdvisories(ctx, cs, res),
		Failures:   collapse.Collapse(rep.Findings),
		Degraded:   rep.Degraded,
	}))
}

// validateSelection rejects mixed selection modes up front: a named target
// is one workload in one namespace, so a selector or --all-namespaces beside
// it silently meaning something else would be worse than an error.
func validateSelection(args []string, selector string, allNamespaces bool) error {
	if len(args) == 0 {
		return nil
	}
	if selector != "" {
		return fmt.Errorf("TYPE/NAME and --selector are mutually exclusive; drop one")
	}
	if allNamespaces {
		return fmt.Errorf("a named target cannot span namespaces; drop TYPE/NAME or --all-namespaces")
	}
	return nil
}

// runFleet is the fan-out path: watch every workload in scope, diagnose the
// non-settled ones, collapse findings across the whole fleet, and emit one
// verdict whose status is the worst outcome observed.
func (o *options) runFleet(ctx context.Context, cs kubernetes.Interface, ns string) error {
	scope := settle.Scope{Namespace: ns, Selector: o.selector, MaxTargets: o.maxTargets, IncludeJobs: o.includeJobs}
	start := time.Now()
	draw, clearLine := o.digStatus("the fleet")
	results, err := settle.RunFleet(ctx, cs, scope, settle.Options{Timeout: o.timeout, StableFor: o.stableFor, WedgedFor: o.wedgedFor, Progress: draw})
	clearLine()
	if err != nil {
		if v, ok := fleetErrorVerdict(err, ns, o.selector); ok {
			return o.emit(v)
		}
		return err
	}

	findings, degraded := diagnoseFleet(ctx, cs, results)
	targets := make([]output.FleetTarget, 0, len(results))
	for _, r := range results {
		targets = append(targets, output.FleetTarget{
			Kind:      string(r.Target.Kind),
			Name:      r.Target.Name,
			Namespace: r.Target.Namespace,
			Status:    statusFor(r.Result.Outcome),
			Reason:    r.Result.Reason,
			Pods:      r.Result.Final.CurrentPods,
			OldPods:   r.Result.Final.OldPods,
		})
	}
	earlyExit := false
	for _, r := range results {
		earlyExit = earlyExit || r.Result.WedgedOut
	}
	var advisories []output.Advisory
	if o.restartWindow > 0 {
		now := time.Now()
		for _, r := range results {
			if r.Result.Outcome != settle.OutcomeSettled {
				continue
			}
			if adv := output.RecentRestarts(r.Result.Final.CurrentPods, o.restartWindow, now); adv != nil {
				adv.Target = fmt.Sprintf("%s/%s", r.Target.Kind, r.Target.Name)
				adv.Namespace = r.Target.Namespace
				adv.Evidence = advisoryEvidence(ctx, cs, r.Result.Final.CurrentPods, o.restartWindow, now)
				advisories = append(advisories, *adv)
			}
		}
	}
	return o.emit(output.BuildFleet(output.FleetInput{
		Namespace:  ns,
		Selector:   o.selector,
		Elapsed:    time.Since(start),
		Targets:    targets,
		Advisories: advisories,
		EarlyExit:  earlyExit,
		WedgedFor:  o.wedgedFor,
		Failures:   collapse.Collapse(findings),
		Degraded:   degraded,
	}))
}

// diagnoseFleet diagnoses every non-settled target, a few at a time, and
// merges the reports in fleet order so the output stays deterministic.
func diagnoseFleet(ctx context.Context, cs kubernetes.Interface, results []settle.TargetResult) ([]signatures.Finding, []string) {
	var unsettled []settle.TargetResult
	for _, r := range results {
		if r.Result.Outcome != settle.OutcomeSettled {
			unsettled = append(unsettled, r)
		}
	}
	reports := make([]signatures.Report, len(unsettled))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, r := range unsettled {
		wg.Add(1)
		go func(slot int, tr settle.TargetResult) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
			defer dcancel()
			ref := signatures.TargetRef{Kind: string(tr.Target.Kind), Namespace: tr.Target.Namespace, Name: tr.Target.Name}
			reports[slot] = signatures.Diagnose(dctx, cs, ref, tr.Result.Final.CurrentPods, tr.Result.Final.OldPods)
		}(i, r)
	}
	wg.Wait()

	var findings []signatures.Finding
	var degraded []string
	seen := map[string]bool{}
	for _, rep := range reports {
		findings = append(findings, rep.Findings...)
		for _, m := range rep.Degraded {
			if seen[m] {
				continue
			}
			seen[m] = true
			degraded = append(degraded, m)
		}
	}
	return findings, degraded
}

// fleetErrorVerdict maps the typed fan-out errors onto structured verdicts:
// an empty match → no_resources_matched (exit 4), an RBAC denial →
// permission_denied (exit 3). An over-ceiling selection stays an error (exit
// 1): the cluster was never checked, so there is no verdict about it.
func fleetErrorVerdict(err error, ns, selector string) (output.Verdict, bool) {
	var nm *settle.NoMatchError
	if errors.As(err, &nm) {
		return output.NoMatchFleet(ns, selector, nm.Error()), true
	}
	var pe *settle.PermissionError
	if errors.As(err, &pe) {
		return output.PermissionDeniedFleet(ns, selector, pe.Error()), true
	}
	return output.Verdict{}, false
}

// errorVerdict maps the typed settle errors onto their structured verdicts:
// not found → no_resources_matched (exit 4), RBAC denial → permission_denied
// (exit 3). Any other error stays an error (exit 1).
func errorVerdict(err error, kind, name, ns string) (output.Verdict, bool) {
	var nf *settle.NotFoundError
	if errors.As(err, &nf) {
		return output.NoMatch(kind, name, ns, nf.Error()), true
	}
	var pe *settle.PermissionError
	if errors.As(err, &pe) {
		return output.PermissionDenied(kind, name, ns, pe.Error()), true
	}
	return output.Verdict{}, false
}

func statusFor(o settle.Outcome) string {
	switch o {
	case settle.OutcomeSettled:
		return output.StatusSettled
	case settle.OutcomeProgressing:
		return output.StatusProgressing
	}
	return output.StatusFailed
}

// settledAdvisories computes the informational notes for a settled
// single-target verdict — today, fresh restart evidence. Non-settled
// verdicts carry diagnosis instead. The workload fields stay empty: the
// verdict header already identifies the target.
func (o *options) settledAdvisories(ctx context.Context, cs kubernetes.Interface, res settle.Result) []output.Advisory {
	if res.Outcome != settle.OutcomeSettled || o.restartWindow <= 0 {
		return nil
	}
	now := time.Now()
	if adv := output.RecentRestarts(res.Final.CurrentPods, o.restartWindow, now); adv != nil {
		adv.Evidence = advisoryEvidence(ctx, cs, res.Final.CurrentPods, o.restartWindow, now)
		return []output.Advisory{*adv}
	}
	return nil
}

// advisoryEvidence fetches the previous-instance log tail behind a fresh
// restart advisory (issue #45) — the crash FreshestTermination names is
// the one the advisory's fields describe, so the evidence and the sentence
// always speak about the same instance. Best-effort and bounded: one log
// read per advisory-carrying target, and a verdict never fails over it.
func advisoryEvidence(ctx context.Context, cs kubernetes.Interface, pods []*corev1.Pod, window time.Duration, now time.Time) []output.Evidence {
	pod, status := output.FreshestTermination(pods, window, now)
	if pod == nil {
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	logs := signatures.CrashLogEvidence(fctx, cs, pod, *status)
	if logs == "" {
		return nil
	}
	return []output.Evidence{{Source: "log", Untrusted: true, Text: logs}}
}

// loadSince parses the --since verdict up front, before any watch: a broken
// state file must fail in the first second, not after the timeout. A missing
// file is not broken — it is the natural first run of a loop, and marks this
// run as the baseline.
func (o *options) loadSince() error {
	if o.since == "" {
		return nil
	}
	b, err := os.ReadFile(o.since)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read --since verdict: %w", err)
	}
	var prev output.Verdict
	if err := json.Unmarshal(b, &prev); err != nil {
		return fmt.Errorf("parse --since verdict %s: %w", o.since, err)
	}
	if prev.SchemaVersion != output.SchemaVersion {
		return fmt.Errorf("--since verdict %s has schemaVersion %q, want %q", o.since, prev.SchemaVersion, output.SchemaVersion)
	}
	o.sinceVerdict = &prev
	return nil
}

// emit trims the verdict to the token budget, writes it in the chosen
// format, and records its exit code.
func (o *options) emit(v output.Verdict) error {
	defer perf.Phase("emit")()
	// Delta rides before the budget: transitions are computed from the full
	// verdict, and the emitted JSON stays a valid --since input for the
	// next run.
	if o.since != "" {
		if o.sinceVerdict == nil {
			v.Delta = &output.Delta{Baseline: true, Transitions: []output.Transition{}}
		} else {
			v.Delta = output.Diff(*o.sinceVerdict, v)
		}
	}
	v = budget.Apply(v, o.budget)
	o.exitCode = v.ExitCode()
	if o.output == "json" {
		return output.WriteJSON(o.streams.Out, v)
	}
	output.WriteText(o.streams.Out, v, o.styler())
	return nil
}

// styler returns the terminal styler, or nil for plain output. Plain wins
// whenever anything says so: --no-color, NO_COLOR, or stdout not being a
// terminal — piped output is always byte-identical plain text.
func (o *options) styler() *output.Styler {
	if o.noColor || os.Getenv("NO_COLOR") != "" {
		return nil
	}
	f, ok := o.streams.Out.(*os.File)
	if !ok {
		return nil
	}
	r := lipgloss.NewRenderer(f)
	if r.ColorProfile() == termenv.Ascii {
		return nil
	}
	return output.NewStyler(r)
}

// digStatus returns a progress callback drawing a live status line on
// stderr, plus its cleanup. Both are no-ops unless stderr is a terminal —
// the status line is display, never output.
func (o *options) digStatus(target string) (func(time.Duration, string), func()) {
	f, ok := o.streams.ErrOut.(*os.File)
	if !ok || o.noColor || lipgloss.NewRenderer(f).ColorProfile() == termenv.Ascii {
		return nil, func() {}
	}
	draw := func(elapsed time.Duration, reason string) {
		line := fmt.Sprintf("⛏ digging %s — %s (%s)", target, reason, elapsed.Round(time.Second))
		if len(line) > 120 {
			line = line[:117] + "..."
		}
		fmt.Fprintf(f, "\r\x1b[K%s", line)
	}
	clear := func() { fmt.Fprintf(f, "\r\x1b[K") }
	return draw, clear
}
