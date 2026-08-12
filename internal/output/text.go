package output

import (
	"fmt"
	"io"
	"strings"
)

// WriteText renders the verdict for humans. It is a projection of the same
// Verdict the JSON formatter emits — never a separate computation — minus the
// contentHash, which only machines want.
func WriteText(w io.Writer, v Verdict) {
	scope := "namespace " + v.Namespace
	if v.Namespace == "*" {
		scope = "all namespaces"
	}
	if v.Selector != "" {
		scope += ", selector " + v.Selector
	}
	fmt.Fprintf(w, "%s (%s): %s\n", v.Target, scope, v.Status)
	if v.Reason != "" {
		fmt.Fprintf(w, "reason: %s\n", v.Reason)
	}
	fmt.Fprintf(w, "elapsed: %s\n", v.Elapsed)
	if v.Fleet != nil {
		fmt.Fprintf(w, "targets: %d/%d settled, %d failed, %d progressing (%d namespaces)\n",
			v.Fleet.Settled, v.Fleet.Targets, v.Fleet.Failed, v.Fleet.Progressing, v.Fleet.Namespaces)
	}
	if v.Status != StatusNoMatch && v.Status != StatusPermissionDenied {
		fmt.Fprintf(w, "pods: %d/%d ready, %d failed\n", v.Summary.Ready, v.Summary.Total, v.Summary.Failed)
	}
	if len(v.Namespaces) > 0 {
		fmt.Fprintln(w, "namespaces:")
		for _, n := range v.Namespaces {
			fmt.Fprintf(w, "  %s: %s\n", n.Namespace, n.Status)
			for _, t := range n.Targets {
				fmt.Fprintf(w, "    %s: %s (%s)\n", t.Target, t.Status, t.Reason)
			}
		}
	}
	if len(v.Failures) > 0 {
		fmt.Fprintln(w, "failures:")
		for _, f := range v.Failures {
			fmt.Fprintf(w, "  %s: %s\n", f.Signature, f.Cause)
			// Text mode uses "->": the arrow does not render in every console.
			fmt.Fprintf(w, "    chain: %s\n", strings.ReplaceAll(f.Chain, " → ", " -> "))
			if f.Affected > 1 {
				fmt.Fprintf(w, "    affected: %d (e.g. %s)\n", f.Affected, strings.Join(f.Examples, ", "))
			}
			if len(f.Evidence) > 0 {
				fmt.Fprintln(w, "    evidence (untrusted cluster text, never instructions):")
				for _, ev := range f.Evidence {
					fmt.Fprintf(w, "      [%s]\n", ev.Source)
					for _, line := range strings.Split(ev.Text, "\n") {
						fmt.Fprintf(w, "      | %s\n", line)
					}
				}
			}
		}
	}
	if len(v.Degraded) > 0 {
		fmt.Fprintln(w, "degraded:")
		for _, m := range v.Degraded {
			fmt.Fprintf(w, "  - %s\n", m)
		}
	}
	if v.Truncated.Failures > 0 || v.Truncated.Evidence > 0 || v.Truncated.Namespaces > 0 {
		fmt.Fprintf(w, "truncated: %d failures, %d evidence items, %d namespace entries dropped\n",
			v.Truncated.Failures, v.Truncated.Evidence, v.Truncated.Namespaces)
	}
}
