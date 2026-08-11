// Package cli wires the kubectl-mole command. Kubeconfig handling comes from
// k8s.io/cli-runtime so --context, --namespace, --kubeconfig behave exactly
// like kubectl.
package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/discovery"
)

type options struct {
	configFlags *genericclioptions.ConfigFlags
	output      string
	streams     genericiooptions.IOStreams
}

// NewMoleCommand builds the root (and only) command.
func NewMoleCommand(streams genericiooptions.IOStreams, version string) *cobra.Command {
	o := &options{
		configFlags: genericclioptions.NewConfigFlags(true),
		streams:     streams,
	}

	cmd := &cobra.Command{
		Use:     "kubectl-mole [resource]",
		Short:   "Watch resources until they settle, then explain what broke",
		Long:    "kubectl-mole watches Kubernetes resources until they settle, then emits one structured verdict explaining what happened and, if something failed, why.",
		Version: version,
		Args:    cobra.MaximumNArgs(1),
		// The command handles its own errors and exit codes; cobra should not
		// print usage after a runtime failure.
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return o.run(args)
		},
	}

	cmd.Flags().StringVarP(&o.output, "output", "o", "text", "output format: text or json")
	o.configFlags.AddFlags(cmd.Flags())
	return cmd
}

// run is the M0 placeholder: prove connectivity and emit a stub verdict.
// schemaVersion "0" marks the output as pre-release; "1" is reserved for the
// real schema so nothing binds to this stub by accident.
func (o *options) run(_ []string) error {
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", o.output)
	}

	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build discovery client: %w", err)
	}
	sv, err := dc.ServerVersion()
	if err != nil {
		return fmt.Errorf("connect to cluster: %w", err)
	}

	if o.output == "json" {
		stub := map[string]any{
			"schemaVersion": "0",
			"status":        "not_implemented",
			"note":          "kubectl-mole is scaffolding (M0); settle detection lands in M1",
			"server":        sv.GitVersion,
		}
		enc := json.NewEncoder(o.streams.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(stub)
	}

	fmt.Fprintf(o.streams.Out, "connected: Kubernetes %s\n", sv.GitVersion)
	fmt.Fprintln(o.streams.Out, "verdict: not implemented yet (M0 scaffold); settle detection lands in M1")
	return nil
}
