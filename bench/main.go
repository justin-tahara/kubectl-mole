// Command bench runs the kubectl-mole benchmark corpus against a disposable
// cluster and emits results.csv and RESULTS.md. The methodology lives in
// bench/README.md; the numbers it produces are the project's headline claim,
// so nothing here may favor mole: baselines run after the failure is steady,
// the density rule is generous to verbose output, and losing scenarios are
// published like the rest.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var (
		kubeContext = flag.String("context", "", "kube context to run against (required; never the ambient current context)")
		molePath    = flag.String("mole", "bin/kubectl-mole", "path to the kubectl-mole binary")
		kstatusPath = flag.String("kubectl-status", "", "path to the kubectl-status binary; empty skips that baseline")
		kstatusVer  = flag.String("kubectl-status-version", "unknown", "version label for the kubectl-status baseline, recorded in RESULTS.md")
		outDir      = flag.String("out", "bench", "directory for results.csv, RESULTS.md, and raw outputs")
		only        = flag.String("only", "", "regexp filter on scenario names")
		full        = flag.Bool("full", false, "include the large fan-out scale points (500 and 5000 namespaces)")
		check       = flag.Bool("check", false, "compare fresh mole output sizes against the committed results.csv instead of overwriting results")
		reportOnly  = flag.Bool("report-only", false, "regenerate RESULTS.md and charts/ from the committed results.csv and meta.json without measuring anything")
	)
	flag.Parse()
	if *reportOnly {
		if err := regenerateReport(*outDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(*kubeContext, *molePath, *kstatusPath, *kstatusVer, *outDir, *only, *full, *check); err != nil {
		log.Fatal(err)
	}
}

// regenerateReport rewrites RESULTS.md and charts/ from the committed
// measurements — presentation changes (report wording, chart styling) need
// no cluster and no re-measurement.
func regenerateReport(outDir string) error {
	rows, err := readRows(filepath.Join(outDir, "results.csv"))
	if err != nil {
		return fmt.Errorf("read committed results: %w", err)
	}
	meta, err := readMeta(filepath.Join(outDir, "meta.json"))
	if err != nil {
		return fmt.Errorf("read committed run meta: %w", err)
	}
	if err := writeMarkdown(filepath.Join(outDir, "RESULTS.md"), rows, meta); err != nil {
		return err
	}
	if err := writeCharts(filepath.Join(outDir, "charts"), rows); err != nil {
		return err
	}
	log.Printf("regenerated %s and %s from committed results", filepath.Join(outDir, "RESULTS.md"), filepath.Join(outDir, "charts"))
	return nil
}

func run(kubeContext, molePath, kstatusPath, kstatusVer, outDir, only string, full, check bool) error {
	if kubeContext == "" {
		return fmt.Errorf("--context is required; the bench never runs against the ambient current context")
	}
	if _, err := os.Stat(molePath); err != nil {
		return fmt.Errorf("mole binary: %w (run make build)", err)
	}
	filter, err := regexp.Compile(only)
	if err != nil {
		return fmt.Errorf("--only: %w", err)
	}

	cs, err := buildClient(kubeContext)
	if err != nil {
		return err
	}
	enc, err := newEncoder(filepath.Join(outDir, ".tiktoken-cache"))
	if err != nil {
		return err
	}
	rc := &runCtx{kubeContext: kubeContext, molePath: molePath, kstatusPath: kstatusPath, enc: enc}

	ctx := context.Background()
	var rows []row
	for _, sc := range corpus() {
		if sc.full && !full {
			continue
		}
		if only != "" && !filter.MatchString(sc.name) {
			continue
		}
		log.Printf("=== %s", sc.name)
		scRows, err := runScenario(ctx, rc, cs, sc, outDir)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", sc.name, err)
		}
		rows = append(rows, scRows...)
	}
	if len(rows) == 0 {
		return fmt.Errorf("no scenarios matched")
	}
	sortRows(rows)

	if check {
		return checkAgainst(filepath.Join(outDir, "results.csv"), rows)
	}

	// A partial (--only) run merges into the committed results instead of
	// clobbering them: re-measuring one scenario costs minutes, not the full
	// corpus. Quote-worthy numbers still come from a full run on a fresh
	// cluster.
	merged := false
	if only != "" {
		committed, err := readRows(filepath.Join(outDir, "results.csv"))
		switch {
		case err == nil:
			var names []string
			for _, sc := range corpus() {
				names = append(names, sc.name)
			}
			rows = mergeRows(committed, rows, names)
			merged = true
		case !os.IsNotExist(err):
			return fmt.Errorf("merge with committed results: %w", err)
		}
	}

	sv := "unknown"
	if v, err := cs.Discovery().ServerVersion(); err == nil {
		sv = v.GitVersion
	}
	meta := runMeta{
		ServerVersion:  sv,
		KstatusVersion: kstatusVer,
		Date:           time.Now().UTC().Format("2006-01-02"),
		Full:           full,
		Merged:         merged,
	}
	if err := writeCSV(filepath.Join(outDir, "results.csv"), rows); err != nil {
		return err
	}
	if err := writeMeta(filepath.Join(outDir, "meta.json"), meta); err != nil {
		return err
	}
	if err := writeMarkdown(filepath.Join(outDir, "RESULTS.md"), rows, meta); err != nil {
		return err
	}
	if err := writeCharts(filepath.Join(outDir, "charts"), rows); err != nil {
		return err
	}
	log.Printf("wrote %s, %s, and %s", filepath.Join(outDir, "results.csv"), filepath.Join(outDir, "RESULTS.md"), filepath.Join(outDir, "charts"))
	return nil
}

func runScenario(ctx context.Context, rc *runCtx, cs *kubernetes.Clientset, sc scenario, outDir string) ([]row, error) {
	f := newFixture(ctx, cs)
	defer f.teardown()
	setupStart := time.Now()
	if err := sc.setup(f); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	setupDur := time.Since(setupStart)
	awaitStart := time.Now()
	if sc.await != nil {
		if err := sc.await(f); err != nil {
			return nil, fmt.Errorf("await: %w", err)
		}
	}
	log.Printf("    staged in %s, converged in %s",
		setupDur.Round(time.Millisecond), time.Since(awaitStart).Round(time.Millisecond))

	// mole first: waiting for the verdict is the tool's own job. The
	// baselines run afterwards, against the steady failure state — they are
	// never penalized for the wait.
	ms := []measurement{rc.mole(f.expandArgs(sc.moleArgs), sc.moleTimeout)}

	if sc.podApp != "" {
		pod, err := f.failingPod(sc.podNS, sc.podApp)
		if err != nil {
			return nil, err
		}
		f.vars["$POD"] = pod
	}
	ms = append(ms, rc.kubectlSeq("kubectl-expert", f.expandSeq(sc.expert)))
	ms = append(ms, rc.kubectlSeq("kubectl-naive", f.expandSeq(sc.naive)))
	if rc.kstatusPath != "" && len(sc.kstatus) > 0 {
		ms = append(ms, rc.kstatusSeq(f.expandSeq(sc.kstatus)))
	}
	if err := writeRaw(filepath.Join(outDir, "raw"), sc.name, ms); err != nil {
		return nil, err
	}

	truth, err := compileTruth(f, sc.truth)
	if err != nil {
		return nil, err
	}
	pertinent, err := pertinentRes(f, sc)
	if err != nil {
		return nil, err
	}

	var rows []row
	for _, m := range ms {
		out := m.combined()
		r := row{
			Scenario:    sc.name,
			Tool:        m.tool,
			Invocations: len(m.invs),
			Bytes:       m.bytes(),
			Tokens:      rc.tokens(out),
			WallMS:      m.wall().Milliseconds(),
			MaxRSSKB:    m.maxRSS(),
		}
		if len(sc.truth) > 0 {
			r.Truth = "no"
			if truthFound(out, truth) {
				r.Truth = "yes"
			}
			if d, ok := rc.density(out, pertinent); ok {
				r.Density = strconv.FormatFloat(d, 'f', 3, 64)
			}
		}
		if m.tool == "mole" {
			est := estTokens(m.bytes())
			r.EstTokens = strconv.Itoa(est)
			if r.Tokens > 0 {
				errPct := (float64(est) - float64(r.Tokens)) / float64(r.Tokens) * 100
				r.EstErrPct = strconv.FormatFloat(round1(errPct), 'f', 1, 64)
			}
			if m.metrics != nil {
				r.APIRequests = strconv.FormatInt(m.metrics.APIRequests, 10)
				r.PreflightMS = msPhase(m.metrics, "preflight")
				r.SyncMS = msPhase(m.metrics, "sync")
				r.WatchMS = msPhase(m.metrics, "watch")
				r.DiagnoseMS = msPhase(m.metrics, "diagnose")
				r.EmitMS = msPhase(m.metrics, "emit")
			}
		}
		log.Printf("    %-16s %2d inv  %8d B  %7d tok  %6d ms  rss=%dKiB api=%s truth=%s density=%s",
			m.tool, r.Invocations, r.Bytes, r.Tokens, r.WallMS, r.MaxRSSKB, dash(r.APIRequests), dash(r.Truth), dash(r.Density))
		rows = append(rows, r)
	}
	return rows, nil
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// buildClient loads kubeconfig for the named context with generous
// client-side rate limits: the harness creates thousands of objects for the
// fan-out scale points.
func buildClient(ctxName string) (*kubernetes.Clientset, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig for context %q: %w", ctxName, err)
	}
	cfg.QPS = 300
	cfg.Burst = 600
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	return cs, nil
}
