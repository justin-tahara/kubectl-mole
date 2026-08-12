package main

import (
	"os"

	"github.com/justin-tahara/kubectl-mole/internal/cli"
	"github.com/justin-tahara/kubectl-mole/internal/perf"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// version is overridden by the release pipeline via -ldflags.
var version = "dev"

func main() {
	perf.Init()
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	code := cli.Execute(streams, version)
	perf.Flush()
	os.Exit(code)
}
