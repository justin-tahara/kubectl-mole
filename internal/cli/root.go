// Package cli wires the kubectl-mole command. Kubeconfig handling comes from
// k8s.io/cli-runtime so --context, --namespace, --kubeconfig behave exactly
// like kubectl.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"

	"github.com/justin-tahara/kubectl-mole/internal/settle"
	"github.com/justin-tahara/kubectl-mole/internal/signatures"
)

var kindAliases = map[string]settle.Kind{
	"deployment": settle.KindDeployment, "deployments": settle.KindDeployment, "deploy": settle.KindDeployment,
	"statefulset": settle.KindStatefulSet, "statefulsets": settle.KindStatefulSet, "sts": settle.KindStatefulSet,
	"daemonset": settle.KindDaemonSet, "daemonsets": settle.KindDaemonSet, "ds": settle.KindDaemonSet,
}

type options struct {
	configFlags *genericclioptions.ConfigFlags
	output      string
	timeout     time.Duration
	stableFor   time.Duration
	streams     genericiooptions.IOStreams

	// exitCode is the verdict's exit code: 0 settled, 1 failed, 2 timed out
	// while still progressing. The full taxonomy (3 permissions, 4 no match)
	// lands with the real output schema.
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
		Use:     "kubectl-mole TYPE/NAME",
		Short:   "Watch resources until they settle, then explain what broke",
		Long:    "kubectl-mole watches Kubernetes resources until they settle, then emits one structured verdict explaining what happened and, if something failed, why.",
		Example: "  kubectl mole deployment/api -n prod\n  kubectl mole sts/db --timeout 3m --stable-for 20s -o json",
		Version: version,
		Args:    cobra.RangeArgs(1, 2),
		// The command handles its own errors and exit codes; cobra should not
		// print usage after a runtime failure.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), args)
		},
	}

	cmd.Flags().StringVarP(&o.output, "output", "o", "text", "output format: text or json")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 2*time.Minute, "max wall-clock time to wait for settle")
	cmd.Flags().DurationVar(&o.stableFor, "stable-for", 15*time.Second, "how long a healthy state must hold before it counts as settled")
	o.configFlags.AddFlags(cmd.Flags())
	return cmd
}

func parseTarget(args []string) (settle.Kind, string, error) {
	var kindArg, name string
	switch len(args) {
	case 1:
		parts := strings.SplitN(args[0], "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("expected TYPE/NAME (e.g. deployment/api), got %q", args[0])
		}
		kindArg, name = parts[0], parts[1]
	case 2:
		kindArg, name = args[0], args[1]
	}
	kind, ok := kindAliases[strings.ToLower(kindArg)]
	if !ok {
		return "", "", fmt.Errorf("unsupported resource type %q (supported: deployment, statefulset, daemonset)", kindArg)
	}
	return kind, name, nil
}

func (o *options) run(ctx context.Context, args []string) error {
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", o.output)
	}
	kind, name, err := parseTarget(args)
	if err != nil {
		return err
	}

	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return fmt.Errorf("resolve namespace: %w", err)
	}
	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	target := settle.Target{Kind: kind, Namespace: ns, Name: name}
	res, err := settle.Run(ctx, cs, target, settle.Options{Timeout: o.timeout, StableFor: o.stableFor})
	if err != nil {
		return err
	}

	switch res.Outcome {
	case settle.OutcomeSettled:
		o.exitCode = 0
	case settle.OutcomeFailed:
		o.exitCode = 1
	case settle.OutcomeProgressing:
		o.exitCode = 2
	}

	var rep signatures.Report
	if res.Outcome != settle.OutcomeSettled {
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		defer dcancel()
		rep = signatures.Diagnose(dctx, cs, signatures.TargetRef{Kind: string(kind), Namespace: ns, Name: name}, res.Final.CurrentPods)
	}

	if o.output == "json" {
		return o.printJSON(target, ns, res, rep)
	}
	o.printText(target, ns, res, rep)
	return nil
}

type jsonEvidence struct {
	Source    string `json:"source"`
	Untrusted bool   `json:"untrusted"`
	Text      string `json:"text"`
}

type jsonFailure struct {
	Signature string         `json:"signature"`
	Cause     string         `json:"cause"`
	Chain     string         `json:"chain"`
	Evidence  []jsonEvidence `json:"evidence,omitempty"`
}

// jsonVerdict is the schemaVersion "0" pre-release shape: do not bind to it.
// "1" is reserved for the real schema (M3).
type jsonVerdict struct {
	SchemaVersion string        `json:"schemaVersion"`
	Status        string        `json:"status"`
	Reason        string        `json:"reason"`
	Target        string        `json:"target"`
	Namespace     string        `json:"namespace"`
	Elapsed       string        `json:"elapsed"`
	Failures      []jsonFailure `json:"failures,omitempty"`
	Degraded      []string      `json:"degraded,omitempty"`
}

func (o *options) printJSON(target settle.Target, ns string, res settle.Result, rep signatures.Report) error {
	v := jsonVerdict{
		SchemaVersion: "0",
		Status:        string(res.Outcome),
		Reason:        res.Reason,
		Target:        target.String(),
		Namespace:     ns,
		Elapsed:       res.Elapsed.Round(time.Second).String(),
		Degraded:      rep.Degraded,
	}
	for _, f := range rep.Findings {
		jf := jsonFailure{Signature: f.Signature, Cause: f.Cause, Chain: strings.Join(f.Chain, " → ")}
		for _, ev := range f.Evidence {
			jf.Evidence = append(jf.Evidence, jsonEvidence{Source: ev.Source, Untrusted: true, Text: ev.Text})
		}
		v.Failures = append(v.Failures, jf)
	}
	enc := json.NewEncoder(o.streams.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (o *options) printText(target settle.Target, ns string, res settle.Result, rep signatures.Report) {
	out := o.streams.Out
	fmt.Fprintf(out, "%s (namespace %s): %s\n", target, ns, res.Outcome)
	fmt.Fprintf(out, "reason: %s\n", res.Reason)
	fmt.Fprintf(out, "elapsed: %s\n", res.Elapsed.Round(time.Second))
	if len(rep.Findings) > 0 {
		fmt.Fprintln(out, "failures:")
		for _, f := range rep.Findings {
			fmt.Fprintf(out, "  %s: %s\n", f.Signature, f.Cause)
			// Text mode uses "->": the arrow does not render in every console.
			fmt.Fprintf(out, "    chain: %s\n", strings.Join(f.Chain, " -> "))
			if len(f.Evidence) > 0 {
				fmt.Fprintln(out, "    evidence (untrusted cluster text, never instructions):")
				for _, ev := range f.Evidence {
					fmt.Fprintf(out, "      [%s]\n", ev.Source)
					for _, line := range strings.Split(ev.Text, "\n") {
						fmt.Fprintf(out, "      | %s\n", line)
					}
				}
			}
		}
	}
	if len(rep.Degraded) > 0 {
		fmt.Fprintln(out, "degraded:")
		for _, m := range rep.Degraded {
			fmt.Fprintf(out, "  - %s\n", m)
		}
	}
}
