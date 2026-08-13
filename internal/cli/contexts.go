package cli

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/justin-tahara/kubectl-mole/internal/collapse"
	"github.com/justin-tahara/kubectl-mole/internal/output"
	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

// This file is the multi-cluster passthrough: --contexts runs the same check
// in several kubeconfig contexts concurrently and merges everything into ONE
// verdict with one exit code. The settle engine is untouched — each context
// gets its own clientset and its own single-target or fleet run, sharing the
// wall clock; diagnosis runs against each context's own cluster. Identical
// causes collapse across clusters, so one bad image rolled everywhere is one
// failure entry, not one per cluster.

// contextOutcome is everything one context contributes to the merged verdict.
type contextOutcome struct {
	name string
	// ns is the namespace this context resolved (its own default unless -n
	// overrode it); "" until known or when the run spans all namespaces.
	ns string
	// entry carries a context-level outcome (unreachable, permission
	// denied, nothing matched) when the context produced no watches; its
	// Status stays "" when targets speak for themselves.
	entry      output.ContextVerdict
	targets    []output.FleetTarget
	advisories []output.Advisory
	findings   []signatures.Finding
	degraded   []string
	earlyExit  bool
}

func (o *options) runContexts(ctx context.Context, args []string) error {
	names, err := o.contextNames()
	if err != nil {
		var nm *noContextsMatchError
		if errors.As(err, &nm) {
			return o.emit(output.NoMatchFleet("", o.selector, nm.Error()))
		}
		return err
	}

	named := len(args) > 0
	var typeArg, name string
	label := "the fleet"
	if named {
		typeArg, name, err = splitTypeName(args)
		if err != nil {
			return err
		}
		label = typeArg + "/" + name
	}
	draw, clearLine := o.digStatus(fmt.Sprintf("%s across %d contexts", label, len(names)))
	var drawMu sync.Mutex
	prefixed := func(ctxName string) func(time.Duration, string) {
		if draw == nil {
			return nil
		}
		return func(elapsed time.Duration, reason string) {
			drawMu.Lock()
			defer drawMu.Unlock()
			draw(elapsed, ctxName+": "+reason)
		}
	}

	start := time.Now()
	outs := make([]contextOutcome, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(slot int, ctxName string) {
			defer wg.Done()
			outs[slot] = o.runOneContext(ctx, ctxName, named, typeArg, name, prefixed(ctxName))
		}(i, n)
	}
	wg.Wait()
	clearLine()

	entries := make([]output.ContextVerdict, len(outs))
	var targets []output.FleetTarget
	var findings []signatures.Finding
	var advisories []output.Advisory
	var degraded []string
	seenDegraded := map[string]bool{}
	earlyExit := false
	for i, out := range outs {
		entries[i] = out.entry
		entries[i].Context = out.name
		targets = append(targets, out.targets...)
		findings = append(findings, out.findings...)
		advisories = append(advisories, out.advisories...)
		for _, m := range out.degraded {
			msg := "context " + out.name + ": " + m
			if seenDegraded[msg] {
				continue
			}
			seenDegraded[msg] = true
			degraded = append(degraded, msg)
		}
		earlyExit = earlyExit || out.earlyExit
	}

	in := output.FleetInput{
		Namespace:  o.mergedNamespace(outs),
		Selector:   o.selector,
		Contexts:   entries,
		Elapsed:    time.Since(start),
		Targets:    targets,
		Advisories: advisories,
		EarlyExit:  earlyExit,
		WedgedFor:  o.wedgedFor,
		Failures:   collapse.Collapse(findings),
		Degraded:   degraded,
	}
	if named {
		if kind, ok := kindAliases[strings.ToLower(typeArg)]; ok {
			in.Target = string(kind) + "/" + name
		} else {
			in.Target = typeArg + "/" + name
		}
	}
	return o.emit(output.BuildFleet(in))
}

// contextNames validates and normalizes --contexts: deduplicated, sorted (so
// the verdict is deterministic regardless of flag order). A literal name
// must exist in the kubeconfig — a typo fails fast before any cluster is
// touched. A glob pattern (*, ?, [) selects from whatever the kubeconfig
// holds — the managed-fleet case where context names rotate — and a pattern
// matching nothing is an empty selection, not a typo: it becomes the
// no_resources_matched verdict, because an empty match must never read as
// success.
func (o *options) contextNames() ([]string, error) {
	if o.configFlags.Context != nil && *o.configFlags.Context != "" {
		return nil, fmt.Errorf("--context and --contexts are mutually exclusive; drop one")
	}
	raw, err := o.configFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	patterns := false
	for _, n := range o.contexts {
		if n == "" {
			continue
		}
		if strings.ContainsAny(n, "*?[") {
			patterns = true
			for name := range raw.Contexts {
				ok, err := path.Match(n, name)
				if err != nil {
					return nil, fmt.Errorf("bad context pattern %q: %w", n, err)
				}
				if ok {
					add(name)
				}
			}
			continue
		}
		if _, ok := raw.Contexts[n]; !ok {
			return nil, fmt.Errorf("context %q not found in kubeconfig", n)
		}
		add(n)
	}
	if len(names) == 0 {
		if patterns {
			return nil, &noContextsMatchError{given: o.contexts}
		}
		return nil, fmt.Errorf("--contexts named no contexts")
	}
	sort.Strings(names)
	return names, nil
}

// noContextsMatchError means every --contexts entry was a pattern and none
// matched a kubeconfig context. The CLI maps it to no_resources_matched
// (exit 4), mirroring the empty-selector rule.
type noContextsMatchError struct {
	given []string
}

func (e *noContextsMatchError) Error() string {
	return fmt.Sprintf("no kubeconfig contexts match %q", strings.Join(e.given, ","))
}

// contextClients builds the clientset for one context. Fresh clientcmd
// machinery per context on purpose: the shared ConfigFlags caches its loader,
// so mutating it would hand every context the first one's cluster.
func (o *options) contextClients(name string) (kubernetes.Interface, *rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.configFlags.KubeConfig != nil && *o.configFlags.KubeConfig != "" {
		rules.ExplicitPath = *o.configFlags.KubeConfig
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: name})
	ns, _, err := loader.Namespace()
	if err != nil {
		return nil, nil, "", err
	}
	// -n applies to every context; without it each context keeps its own
	// default namespace, exactly like running kubectl against each.
	if o.configFlags.Namespace != nil && *o.configFlags.Namespace != "" {
		ns = *o.configFlags.Namespace
	}
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, nil, "", err
	}
	cfg.QPS = o.qps
	cfg.Burst = o.burst
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, "", err
	}
	return cs, cfg, ns, nil
}

// runOneContext runs the selected check (named target or fleet) in a single
// context. Errors never escape: a context that cannot be checked becomes a
// context-level entry in the merged verdict, and the other contexts keep
// going — one flaky cluster must not erase the verdict about the rest.
func (o *options) runOneContext(ctx context.Context, ctxName string, named bool, typeArg, name string, progress func(time.Duration, string)) contextOutcome {
	out := contextOutcome{name: ctxName}
	cs, cfg, ns, err := o.contextClients(ctxName)
	if err != nil {
		out.entry = output.ContextVerdict{Status: output.StatusFailed, Reason: "cannot build client: " + err.Error()}
		return out
	}
	if o.allNamespaces {
		ns = ""
	}
	out.ns = ns
	opts := settle.Options{Timeout: o.timeout, StableFor: o.stableFor, WedgedFor: o.wedgedFor, Progress: progress}

	var results []settle.TargetResult
	if !named {
		scope := settle.Scope{Namespace: ns, Selector: o.selector, MaxTargets: o.maxTargets, IncludeJobs: o.includeJobs}
		results, err = settle.RunFleet(ctx, cs, scope, opts)
	} else if kind, ok := kindAliases[strings.ToLower(typeArg)]; ok {
		target := settle.Target{Kind: kind, Namespace: ns, Name: name}
		var res settle.Result
		res, err = settle.Run(ctx, cs, target, opts)
		results = []settle.TargetResult{{Target: target, Result: res}}
	} else {
		var res settle.Result
		var target settle.Target
		res, target, err = o.runCustomContext(ctx, cs, cfg, typeArg, name, ns, opts)
		results = []settle.TargetResult{{Target: target, Result: res}}
	}
	if err != nil {
		out.entry = contextErrorEntry(err)
		return out
	}
	out.fill(o, ctx, cs, results)
	return out
}

// runCustomContext is the per-context flavor of the dynamic-engine path:
// type resolution runs against this context's own discovery — clusters
// legitimately differ in which CRDs they carry.
func (o *options) runCustomContext(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, typeArg, name, ns string, opts settle.Options) (settle.Result, settle.Target, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return settle.Result{}, settle.Target{}, fmt.Errorf("build discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	gvr, gvk, err := resolveType(mapper, typeArg)
	if err != nil {
		return settle.Result{}, settle.Target{}, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return settle.Result{}, settle.Target{}, fmt.Errorf("build dynamic client: %w", err)
	}
	res, err := settle.RunCustom(ctx, cs, dyn, gvr, gvk.Kind, ns, name, opts)
	if err != nil {
		return settle.Result{}, settle.Target{}, err
	}
	return res, settle.Target{Kind: settle.Kind(gvk.Kind), Namespace: ns, Name: name}, nil
}

// fill converts one context's watch results into the merged verdict's
// currency: context-tagged fleet targets, diagnosis against this context's
// own cluster, and restart advisories prefixed with the context.
func (out *contextOutcome) fill(o *options, ctx context.Context, cs kubernetes.Interface, results []settle.TargetResult) {
	for _, r := range results {
		out.targets = append(out.targets, output.FleetTarget{
			Kind:      string(r.Target.Kind),
			Name:      r.Target.Name,
			Namespace: r.Target.Namespace,
			Context:   out.name,
			Status:    statusFor(r.Result.Outcome),
			Reason:    r.Result.Reason,
			Pods:      r.Result.Final.CurrentPods,
			OldPods:   r.Result.Final.OldPods,
		})
		out.earlyExit = out.earlyExit || r.Result.WedgedOut
	}
	findings, degraded := diagnoseFleet(ctx, cs, results)
	for i := range findings {
		findings[i].Context = out.name
	}
	out.findings = findings
	out.degraded = degraded
	if o.restartWindow <= 0 {
		return
	}
	now := time.Now()
	for _, r := range results {
		if r.Result.Outcome != settle.OutcomeSettled {
			continue
		}
		if adv := output.RecentRestarts(r.Result.Final.CurrentPods, o.restartWindow, now); adv != nil {
			adv.Context = out.name
			adv.Target = fmt.Sprintf("%s/%s", r.Target.Kind, r.Target.Name)
			adv.Namespace = r.Target.Namespace
			out.advisories = append(out.advisories, *adv)
		}
	}
}

// contextErrorEntry maps one context's typed errors onto its rollup entry,
// mirroring what the single-cluster CLI maps onto whole verdicts. Anything
// untyped — an unreachable API server, an over-ceiling selection — reports
// the context failed with the error as the reason.
func contextErrorEntry(err error) output.ContextVerdict {
	var nf *settle.NotFoundError
	var nm *settle.NoMatchError
	if errors.As(err, &nf) || errors.As(err, &nm) {
		return output.ContextVerdict{Status: output.StatusNoMatch, Reason: err.Error()}
	}
	var pe *settle.PermissionError
	if errors.As(err, &pe) {
		return output.ContextVerdict{Status: output.StatusPermissionDenied, Reason: err.Error()}
	}
	return output.ContextVerdict{Status: output.StatusFailed, Reason: "watch failed: " + err.Error()}
}

// mergedNamespace picks the verdict's namespace field: -n wins everywhere;
// otherwise the contexts' own defaults, when they all agree. Disagreeing
// defaults render as "*" — the per-target entries carry the exact one.
func (o *options) mergedNamespace(outs []contextOutcome) string {
	if o.allNamespaces {
		return ""
	}
	if o.configFlags.Namespace != nil && *o.configFlags.Namespace != "" {
		return *o.configFlags.Namespace
	}
	ns := ""
	for _, out := range outs {
		switch {
		case out.ns == "":
		case ns == "":
			ns = out.ns
		case ns != out.ns:
			return ""
		}
	}
	return ns
}
