package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// row is one (scenario, tool) measurement, the unit the CSV and the markdown
// table share.
type row struct {
	Scenario    string
	Tool        string
	Invocations int
	Bytes       int
	Tokens      int
	WallMS      int64
	// Truth is "yes"/"no", or "" for control scenarios with no failure.
	Truth string
	// Density is a formatted fraction, or "" when there is no failure to
	// pertain to.
	Density string
	// EstTokens/EstErrPct hold mole's own ~4 chars/token estimate and its
	// signed error against the real tokenizer; only mole rows carry them.
	EstTokens string
	EstErrPct string
	// MaxRSSKB is the peak resident set across the tool's invocations, KiB.
	MaxRSSKB int64
	// APIRequests and the phase columns come from mole's self-metrics
	// (MOLE_METRICS_FILE); only mole rows carry them. The watch phase is the
	// deliberate wait; everything else is overhead.
	APIRequests string
	PreflightMS string
	SyncMS      string
	WatchMS     string
	DiagnoseMS  string
	EmitMS      string
}

// baseColumns is the pre-resource-metrics schema; committed files that old
// are still readable, with the extra columns defaulting to absent.
const baseColumns = 10

var csvHeader = []string{
	"scenario", "tool", "invocations", "bytes", "tokens", "wall_ms",
	"truth_found", "signal_density", "mole_est_tokens", "mole_est_error_pct",
	"max_rss_kb", "api_requests",
	"preflight_ms", "sync_ms", "watch_ms", "diagnose_ms", "emit_ms",
}

func (r row) record() []string {
	return []string{
		r.Scenario, r.Tool,
		strconv.Itoa(r.Invocations), strconv.Itoa(r.Bytes), strconv.Itoa(r.Tokens),
		strconv.FormatInt(r.WallMS, 10), r.Truth, r.Density, r.EstTokens, r.EstErrPct,
		strconv.FormatInt(r.MaxRSSKB, 10), r.APIRequests,
		r.PreflightMS, r.SyncMS, r.WatchMS, r.DiagnoseMS, r.EmitMS,
	}
}

func writeCSV(path string, rows []row) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	if err := w.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write(r.record()); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

type runMeta struct {
	ServerVersion  string
	KstatusVersion string
	Date           string
	Full           bool
	// Merged marks a partial (--only) run whose unmeasured rows were carried
	// over from the committed results.
	Merged bool
}

// readRows loads a results CSV back into rows, so a partial re-measurement
// can merge into the committed results instead of clobbering them.
func readRows(path string) ([]row, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	records, err := csv.NewReader(fh).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []row
	for i, rec := range records {
		if i == 0 {
			continue
		}
		if len(rec) < baseColumns {
			return nil, fmt.Errorf("%s line %d: %d fields, want at least %d", path, i+1, len(rec), baseColumns)
		}
		inv, err1 := strconv.Atoi(rec[2])
		bs, err2 := strconv.Atoi(rec[3])
		tok, err3 := strconv.Atoi(rec[4])
		wall, err4 := strconv.ParseInt(rec[5], 10, 64)
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		r := row{
			Scenario: rec[0], Tool: rec[1], Invocations: inv, Bytes: bs, Tokens: tok,
			WallMS: wall, Truth: rec[6], Density: rec[7], EstTokens: rec[8], EstErrPct: rec[9],
		}
		if len(rec) >= len(csvHeader) {
			rss, err := strconv.ParseInt(rec[10], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s line %d max_rss_kb: %w", path, i+1, err)
			}
			r.MaxRSSKB = rss
			r.APIRequests, r.PreflightMS, r.SyncMS = rec[11], rec[12], rec[13]
			r.WatchMS, r.DiagnoseMS, r.EmitMS = rec[14], rec[15], rec[16]
		}
		out = append(out, r)
	}
	return out, nil
}

// mergeRows replaces the committed rows of just-measured scenarios and keeps
// everything else. Output follows corpus order so a partial run cannot
// reshuffle the file; committed scenarios that left the corpus go last,
// visibly, rather than vanishing.
func mergeRows(committed, fresh []row, corpusNames []string) []row {
	measured := map[string]bool{}
	for _, r := range fresh {
		measured[r.Scenario] = true
	}
	byScenario := map[string][]row{}
	var stale []string
	inCorpus := map[string]bool{}
	for _, name := range corpusNames {
		inCorpus[name] = true
	}
	for _, r := range committed {
		if measured[r.Scenario] {
			continue
		}
		if len(byScenario[r.Scenario]) == 0 && !inCorpus[r.Scenario] {
			stale = append(stale, r.Scenario)
		}
		byScenario[r.Scenario] = append(byScenario[r.Scenario], r)
	}
	for _, r := range fresh {
		byScenario[r.Scenario] = append(byScenario[r.Scenario], r)
	}
	var out []row
	for _, name := range corpusNames {
		out = append(out, byScenario[name]...)
	}
	for _, name := range stale {
		out = append(out, byScenario[name]...)
	}
	return out
}

func writeMarkdown(path string, rows []row, meta runMeta) error {
	var b strings.Builder
	b.WriteString("# Benchmark results\n\n")
	b.WriteString("Generated by `make bench` — do not edit. Methodology: [README.md](README.md).\n\n")
	fmt.Fprintf(&b, "- Kubernetes: %s (kind, pinned image in [kind.yaml](kind.yaml))\n", meta.ServerVersion)
	fmt.Fprintf(&b, "- kubectl-status: %s\n", meta.KstatusVersion)
	fmt.Fprintf(&b, "- Tokens: tiktoken `o200k_base`; bytes always published alongside\n")
	fmt.Fprintf(&b, "- Wall clock is proof versus photo: mole's tool overhead is under 100ms (see the per-scenario phase lines) — the rest of its wall is the evidence window. The snapshot baselines are timed after the failure is already steady, so the wait that got the operator there is not in their column.\n")
	if meta.Merged {
		fmt.Fprintf(&b, "- Date: %s (partial re-measurement merged into committed results; per-scenario provenance in git history)\n\n", meta.Date)
	} else {
		fmt.Fprintf(&b, "- Date: %s\n\n", meta.Date)
	}

	scenarios := orderedScenarios(rows)

	b.WriteString("## Output tokens to reach a verdict\n\n")
	b.WriteString("| Scenario | mole | expert kubectl | naive kubectl | kubectl-status |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, sc := range scenarios {
		byTool := toolMap(rows, sc)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", sc,
			cell(byTool["mole"]), cell(byTool["kubectl-expert"]),
			cell(byTool["kubectl-naive"]), cell(byTool["kubectl-status"]))
	}
	b.WriteString("\nA cell is `tokens (✓)` when the output contains the ground-truth cause, `(✗)` when it does not. Control scenarios have no cause to find.\n")

	b.WriteString("\n## Full measurements\n")
	for _, sc := range scenarios {
		fmt.Fprintf(&b, "\n### %s\n\n", sc)
		b.WriteString("| Tool | Invocations | Bytes | Tokens | Wall ms | Max RSS | API reqs | Truth | Signal density |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, r := range rows {
			if r.Scenario != sc {
				continue
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s | %s | %s | %s |\n",
				r.Tool, r.Invocations, r.Bytes, r.Tokens, r.WallMS,
				rssCell(r.MaxRSSKB), dash(r.APIRequests),
				dash(r.Truth), dash(r.Density))
		}
		for _, r := range rows {
			if r.Scenario == sc && r.Tool == "mole" && r.EstTokens != "" {
				fmt.Fprintf(&b, "\nmole's own ~4 chars/token estimate: %s tokens (%s%% vs o200k_base).\n",
					r.EstTokens, r.EstErrPct)
			}
			if r.Scenario == sc && r.Tool == "mole" && r.WatchMS != "" {
				fmt.Fprintf(&b, "\nmole phases (ms): preflight %s, informer sync %s, watch %s (the deliberate wait), diagnose %s, emit %s.\n",
					dash(r.PreflightMS), dash(r.SyncMS), dash(r.WatchMS), dash(r.DiagnoseMS), dash(r.EmitMS))
			}
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func cell(r *row) string {
	if r == nil {
		return "—"
	}
	mark := ""
	switch r.Truth {
	case "yes":
		mark = " (✓)"
	case "no":
		mark = " (✗)"
	}
	return fmt.Sprintf("%d%s", r.Tokens, mark)
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func rssCell(kb int64) string {
	if kb <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f MiB", float64(kb)/1024)
}

func orderedScenarios(rows []row) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Scenario] {
			seen[r.Scenario] = true
			out = append(out, r.Scenario)
		}
	}
	return out
}

func toolMap(rows []row, scenario string) map[string]*row {
	out := map[string]*row{}
	for i := range rows {
		if rows[i].Scenario == scenario {
			out[rows[i].Tool] = &rows[i]
		}
	}
	return out
}

// checkAgainst is the CI mode: fresh mole output must not have grown beyond
// the committed results by more than the threshold. Only mole is checked —
// the baselines are somebody else's output.
func checkAgainst(committedCSV string, fresh []row) error {
	committed, err := readCommitted(committedCSV)
	if err != nil {
		return fmt.Errorf("read committed results: %w", err)
	}
	var failures []string
	for _, r := range fresh {
		if r.Tool != "mole" {
			continue
		}
		old, ok := committed[r.Scenario]
		if !ok {
			fmt.Printf("check: %-24s NEW (no committed baseline, %d bytes)\n", r.Scenario, r.Bytes)
			continue
		}
		limit := old + old*30/100 + 256
		status := "ok"
		if r.Bytes > limit {
			status = "GREW"
			failures = append(failures,
				fmt.Sprintf("%s: mole output grew to %d bytes (committed %d, limit %d)", r.Scenario, r.Bytes, old, limit))
		}
		fmt.Printf("check: %-24s %s (%d bytes, committed %d, limit %d)\n", r.Scenario, status, r.Bytes, old, limit)
	}
	if len(failures) > 0 {
		return fmt.Errorf("output growth over threshold:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}

func readCommitted(path string) (map[string]int, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	records, err := csv.NewReader(fh).ReadAll()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for i, rec := range records {
		if i == 0 || len(rec) < 4 || rec[1] != "mole" {
			continue
		}
		n, err := strconv.Atoi(rec[3])
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		out[rec[0]] = n
	}
	return out, nil
}

// writeRaw dumps every invocation's output for scrutiny; the directory is
// gitignored.
func writeRaw(dir, scenario string, ms []measurement) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, m := range ms {
		var b strings.Builder
		for _, inv := range m.invs {
			fmt.Fprintf(&b, "$ %s\n", strings.Join(inv.argv, " "))
			b.Write(inv.out)
			fmt.Fprintf(&b, "\n[%s, max RSS %d KiB]\n\n", inv.dur.Round(time.Millisecond), inv.maxRSSKB)
		}
		name := filepath.Join(dir, fmt.Sprintf("%s.%s.txt", scenario, m.tool))
		if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// sortRows keeps CSV ordering deterministic: corpus order is preserved via
// first appearance, tools in a fixed order within a scenario.
var toolOrder = map[string]int{"mole": 0, "kubectl-expert": 1, "kubectl-naive": 2, "kubectl-status": 3}

func sortRows(rows []row) {
	scenarioIdx := map[string]int{}
	for _, r := range rows {
		if _, ok := scenarioIdx[r.Scenario]; !ok {
			scenarioIdx[r.Scenario] = len(scenarioIdx)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if scenarioIdx[rows[i].Scenario] != scenarioIdx[rows[j].Scenario] {
			return scenarioIdx[rows[i].Scenario] < scenarioIdx[rows[j].Scenario]
		}
		return toolOrder[rows[i].Tool] < toolOrder[rows[j].Tool]
	})
}
