package output

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestTextGoldens locks the text output byte for byte in both modes. Plain
// is the contract piped consumers depend on; styled is rendered with a
// forced ANSI profile so the golden is stable on any machine, terminal or
// not.
func TestTextGoldens(t *testing.T) {
	styled := lipgloss.NewRenderer(io.Discard)
	styled.SetColorProfile(termenv.ANSI)

	cases := []struct {
		name   string
		styler *Styler
	}{
		{"plain", nil},
		{"styled", NewStyler(styled)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			WriteText(&buf, Build(failedInput()), tc.styler)
			path := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Fatalf("output drifted from %s:\n--- got ---\n%s\n--- want ---\n%s", path, buf.Bytes(), want)
			}
		})
	}
}

// TestStyledAddsOnlyEscapes proves the styler contract: stripping the escape
// codes from styled output must yield the plain output exactly.
func TestStyledAddsOnlyEscapes(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.ANSI)

	var plain, styled bytes.Buffer
	v := Build(failedInput())
	WriteText(&plain, v, nil)
	WriteText(&styled, v, NewStyler(r))

	stripped := stripANSI(styled.Bytes())
	if !bytes.Equal(stripped, plain.Bytes()) {
		t.Fatalf("styled output is not plain + escapes:\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain.Bytes())
	}
}

// stripANSI removes CSI escape sequences.
func stripANSI(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b); i++ {
		if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '[' {
			i += 2
			for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
				i++
			}
			continue
		}
		out = append(out, b[i])
	}
	return out
}
