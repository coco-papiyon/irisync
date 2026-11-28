package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/coco-papiyon/irisync/internal/remote"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stdio := flags.Bool("stdio", false, "serve the remote scan protocol on standard input and output")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "agent: unexpected positional arguments")
		return 2
	}
	if *stdio {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		if err := remote.Serve(ctx, os.Stdin, os.Stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "agent: stdio session failed")
			return 1
		}
	}
	return 0
}
