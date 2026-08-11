package main

import (
	"os"

	"github.com/justin-tahara/kubectl-mole/internal/cli"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// version is overridden by the release pipeline via -ldflags.
var version = "dev"

func main() {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	os.Exit(cli.Execute(streams, version))
}
