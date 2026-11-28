package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/remote"
)

func runRemoteScan(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("controller scan-remote", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "load YAML configuration from this path")
	id := flags.String("pair", "", "scan the remote root of this pair")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *configPath == "" || *id == "" {
		fmt.Fprintln(stderr, "controller scan-remote: --config and --pair are required")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "controller scan-remote:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	for _, pair := range cfg.Pairs {
		if pair.ID != *id {
			continue
		}
		result, err := remote.Scan(ctx, pair.SSH, pair.RemotePath)
		if err != nil {
			fmt.Fprintln(stderr, "controller scan-remote:", err)
			return 1
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintln(stderr, "controller scan-remote: could not write scan result")
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "controller scan-remote: pair not found")
	return 1
}
