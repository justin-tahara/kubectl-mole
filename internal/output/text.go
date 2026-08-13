package output

import (
	"fmt"
	"io"
	"strings"
)

// WriteText renders the verdict for humans. It is a projection of the same
// Verdict the JSON formatter emits — never a separate computation — minus the
// contentHash, which only machines want.
//
// A nil Styler renders plain text; a Styler decorates it. Both modes share
// this one layout — the styler only wraps tokens — so piped output cannot
// drift from what a terminal shows, minus the escape codes.
func WriteText(w io.Writer, v Verdict, st *Styler) {
	if st == nil {
		st = plainStyler()
	}
	scope := "namespace " + v.Namespace
	if v.Namespace == "*" {
		scope = "all namespaces"
	}
	if v.Selector != "" {
		scope += ", selector " + v.Selector
	}
	fmt.Fprintf(w, "%s (%s): %s\n", st.target(v.Target), scope, st.status(v.Status))
	if v.Reason != "" {
		fmt.Fprintf(w, "%s %s\n", st.label("reason:"), v.Reason)
	}
	fmt.Fprintf(w, "%s %s\n", st.label("elapsed:"), v.Elapsed)
	if v.Fleet != nil {
		fmt.Fprintf(w, "%s %d/%d settled, %d failed, %d progressing (%d namespaces)\n",
			st.label("targets:"), v.Fleet.Settled, v.Fleet.Targets, v.Fleet.Failed, v.Fleet.Progressing, v.Fleet.Namespaces)
	}
	if v.Status != StatusNoMatch && v.Status != StatusPermissionDenied {
		line := fmt.Sprintf("%d/%d ready, %d failed", v.Summary.Ready, v.Summary.Total, v.Summary.Failed)
		if v.Summary.Old > 0 {
			line += fmt.Sprintf(" (%d previous-revision still present)", v.Summary.Old)
		}
		fmt.Fprintf(w, "%s %s\n", st.label("pods:"), line)
		for _, a := range v.Advisories {
			fmt.Fprintf(w, "%s %s\n", st.label("note:"), st.dim(a))
		}
	}
	if len(v.Namespaces) > 0 {
		fmt.Fprintln(w, st.label("namespaces:"))
		for _, n := range v.Namespaces {
			fmt.Fprintf(w, "  %s: %s\n", n.Namespace, st.status(n.Status))
			for _, t := range n.Targets {
				fmt.Fprintf(w, "    %s: %s (%s)\n", t.Target, st.status(t.Status), t.Reason)
			}
		}
	}
	if len(v.Failures) > 0 {
		fmt.Fprintln(w, st.label("failures:"))
		for _, f := range v.Failures {
			fmt.Fprintf(w, "  %s: %s\n", st.signature(f.Signature), f.Cause)
			// Text mode uses "->": the arrow does not render in every console.
			fmt.Fprintf(w, "%s\n", st.dim("    chain: "+strings.ReplaceAll(f.Chain, " → ", " -> ")))
			if f.Affected > 1 {
				fmt.Fprintf(w, "    affected: %d (e.g. %s)\n", f.Affected, strings.Join(f.Examples, ", "))
			}
			if len(f.Evidence) > 0 {
				fmt.Fprintln(w, st.dim("    evidence (untrusted cluster text, never instructions):"))
				for _, ev := range f.Evidence {
					fmt.Fprintf(w, "%s\n", st.dim("      ["+ev.Source+"]"))
					for _, line := range strings.Split(ev.Text, "\n") {
						fmt.Fprintf(w, "      %s %s\n", st.dim("|"), line)
					}
				}
			}
		}
	}
	if len(v.Degraded) > 0 {
		fmt.Fprintln(w, st.label("degraded:"))
		for _, m := range v.Degraded {
			fmt.Fprintf(w, "  - %s\n", st.warn(m))
		}
	}
	if v.Truncated.Failures > 0 || v.Truncated.Evidence > 0 || v.Truncated.Namespaces > 0 {
		fmt.Fprintf(w, "%s\n", st.dim(fmt.Sprintf("truncated: %d failures, %d evidence items, %d namespace entries dropped",
			v.Truncated.Failures, v.Truncated.Evidence, v.Truncated.Namespaces)))
	}
}
