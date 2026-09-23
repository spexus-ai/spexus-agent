package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spexus-ai/spexus-agent/internal/swarmrunner"
)

func main() {
	configPath := flag.String("config", "", "runner JSON config")
	history := flag.Bool("history", false, "read safe journal metadata")
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "--config is required")
		os.Exit(2)
	}
	c, e := swarmrunner.LoadConfig(*configPath)
	if e != nil {
		fail(e)
	}
	if *history {
		h, e := swarmrunner.ReadHistory(c.StateDirectory)
		if e != nil {
			fail(e)
		}
		if e = json.NewEncoder(os.Stdout).Encode(h); e != nil {
			fail(e)
		}
		return
	}
	r, e := swarmrunner.New(c)
	if e != nil {
		fail(e)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	e = r.Run(ctx)
	closeErr := r.Close()
	if e != nil && !errors.Is(e, context.Canceled) {
		fail(e)
	}
	if closeErr != nil {
		fail(closeErr)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
