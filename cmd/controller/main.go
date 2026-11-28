package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/coco-papiyon/irisync/internal/config"
	"github.com/coco-papiyon/irisync/internal/state"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var configPath string
	var hasConfig bool
	var statePath string
	var hasStatePath bool
	flags.Func("state-db", "override the SQLite state database path", func(path string) error {
		statePath = path
		hasStatePath = true
		return nil
	})
	flags.Func("config", "load YAML configuration from this path", func(path string) error {
		configPath = path
		hasConfig = true
		return nil
	})
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "controller: unexpected positional arguments")
		return 2
	}
	if hasStatePath && (!hasConfig || statePath == "") {
		fmt.Fprintln(stderr, "controller: --state-db needs a path and --config")
		return 2
	}
	if !hasConfig {
		return 0
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, "controller:", err)
		return 1
	}
	if !hasStatePath {
		statePath = configPath + ".state.sqlite3"
	}
	ctx := context.Background()
	path, err := databasePath(statePath, cfg.Pairs)
	if err != nil {
		fmt.Fprintln(stderr, "controller:", err)
		return 1
	}
	store, err := state.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(stderr, "controller: open state database:", withoutPath(err))
		return 1
	}
	_, err = restorePairs(ctx, store, cfg.Pairs)
	if err == nil {
		err = store.Close()
	} else {
		_ = store.Close()
	}
	if err != nil {
		fmt.Fprintln(stderr, "controller: restore state:", withoutPath(err))
		return 1
	}
	return 0
}
