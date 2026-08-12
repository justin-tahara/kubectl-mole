package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

// invocation is one command run and what came back. Output is stdout and
// stderr combined: an error message is output the consumer has to read too.
type invocation struct {
	argv []string
	out  []byte
	dur  time.Duration
}

// measurement is one tool's complete attempt at a scenario.
type measurement struct {
	tool string
	invs []invocation
}

func (m measurement) combined() string {
	var b strings.Builder
	for _, inv := range m.invs {
		b.Write(inv.out)
		b.WriteByte('\n')
	}
	return b.String()
}

func (m measurement) bytes() int {
	n := 0
	for _, inv := range m.invs {
		n += len(inv.out)
	}
	return n
}

func (m measurement) wall() time.Duration {
	var d time.Duration
	for _, inv := range m.invs {
		d += inv.dur
	}
	return d
}

// estTokens mirrors mole's own budget estimate (internal/budget): ~3
// characters per token, tokenizer-free. The bench publishes its error
// against the real tokenizer.
func estTokens(bytes int) int {
	return (bytes + 2) / 3
}

// runCtx holds what every tool invocation needs.
type runCtx struct {
	kubeContext string
	molePath    string
	kstatusPath string
	enc         *tiktoken.Tiktoken
}

// execOne runs a command to completion with a hard ceiling, measuring wall
// clock and capturing combined output. A failing command is a valid
// measurement: its error text is what the consumer would have to read.
func execOne(argv []string) invocation {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, _ := cmd.CombinedOutput()
	return invocation{argv: argv, out: out, dur: time.Since(start)}
}

// mole runs the tool under test: one invocation, JSON output, its own watch.
func (rc *runCtx) mole(args []string, timeout time.Duration) measurement {
	argv := append([]string{rc.molePath}, args...)
	argv = append(argv, "-o", "json", "--stable-for", "5s",
		"--timeout", timeout.String(), "--context", rc.kubeContext)
	return measurement{tool: "mole", invs: []invocation{execOne(argv)}}
}

func (rc *runCtx) kubectlSeq(tool string, seq [][]string) measurement {
	m := measurement{tool: tool}
	for _, args := range seq {
		argv := append([]string{"kubectl"}, args...)
		argv = append(argv, "--context", rc.kubeContext)
		m.invs = append(m.invs, execOne(argv))
	}
	return m
}

func (rc *runCtx) kstatusSeq(seq [][]string) measurement {
	m := measurement{tool: "kubectl-status"}
	for _, args := range seq {
		argv := append([]string{rc.kstatusPath}, args...)
		argv = append(argv, "--context", rc.kubeContext)
		m.invs = append(m.invs, execOne(argv))
	}
	return m
}

// newEncoder loads the o200k_base tokenizer, caching its BPE file under
// cacheDir so repeat runs are offline.
func newEncoder(cacheDir string) (*tiktoken.Tiktoken, error) {
	if os.Getenv("TIKTOKEN_CACHE_DIR") == "" {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.Setenv("TIKTOKEN_CACHE_DIR", cacheDir); err != nil {
			return nil, err
		}
	}
	enc, err := tiktoken.GetEncoding("o200k_base")
	if err != nil {
		return nil, fmt.Errorf("load o200k_base tokenizer (network needed on first run): %w", err)
	}
	return enc, nil
}

func (rc *runCtx) tokens(s string) int {
	return len(rc.enc.Encode(s, nil, nil))
}

// truthFound reports whether every ground-truth regex matches the combined
// output, case-insensitively. All-must-match keeps a lucky keyword from
// counting as the answer.
func truthFound(out string, truth []*regexp.Regexp) bool {
	for _, re := range truth {
		if !re.MatchString(out) {
			return false
		}
	}
	return true
}

// density is the fraction of output tokens on lines that mention the
// failure — the failing workload, pod, node, or cause — versus everything
// else. The rule is deliberately mechanical: it overcounts pertinence if
// anything (a healthy-pod line naming the workload still counts), so it is
// generous to the verbose baselines, never to mole.
func (rc *runCtx) density(out string, pertinent []*regexp.Regexp) (float64, bool) {
	total, hit := 0, 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := len(rc.enc.Encode(line, nil, nil))
		total += n
		for _, re := range pertinent {
			if re.MatchString(line) {
				hit += n
				break
			}
		}
	}
	if total == 0 {
		return 0, false
	}
	return float64(hit) / float64(total), true
}

// compileTruth expands fixture variables into the truth patterns and
// compiles them case-insensitive. Variable values are quoted literally.
func compileTruth(f *fixture, patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		expanded := expandQuoted(f, p)
		re, err := regexp.Compile("(?i)" + expanded)
		if err != nil {
			return nil, fmt.Errorf("truth pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// signatureNames matches any signature from the catalogue: a line stating
// the class of failure pertains to the failure, whichever tool printed it.
const signatureNames = `imagepullbackoff|crashloopbackoff|podunschedulable|pvcpending|oomkilled|probefailing|admissionrejected|quotaexceeded|nodenotready|configmissing|createcontainerconfigerror|volumemountfailed|failedmount|podsandboxfailed|containerstartfailed|starterror|podevicted|evicted|podstuckterminating`

// pertinentRes builds the density matcher: a line pertains to the failure
// when it names a failing resource, states the ground-truth cause, or names
// the failure class. The same rule applies to every tool.
func pertinentRes(f *fixture, sc scenario) ([]*regexp.Regexp, error) {
	patterns := append([]string{signatureNames}, sc.pertinent...)
	patterns = append(patterns, sc.truth...)
	if !sc.fleetGeneric {
		for _, term := range f.pertinent {
			patterns = append(patterns, regexp.QuoteMeta(term))
		}
	}
	for _, key := range []string{"$POD", "$NODE", "$NSFAIL", "$NSFAIL2", "$NSFAIL3"} {
		if v, ok := f.vars[key]; ok {
			patterns = append(patterns, regexp.QuoteMeta(v))
		}
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + expandQuoted(f, p))
		if err != nil {
			return nil, fmt.Errorf("pertinent pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// expandQuoted substitutes fixture variables into a regex source with the
// values quoted as literals.
func expandQuoted(f *fixture, s string) string {
	keys := make([]string, 0, len(f.vars))
	for k := range f.vars {
		keys = append(keys, k)
	}
	// Longest first so $NSFAIL is never half-eaten by $NS.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if len(keys[j]) > len(keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, regexp.QuoteMeta(f.vars[k]))
	}
	return s
}
