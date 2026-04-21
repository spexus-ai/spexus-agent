package main

import (
	"os"

	"github.com/spexus-ai/spexus-agent/internal/cli"
)

func main() {
	os.Exit(cli.New().Run(os.Args[1:]))
}
