package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/coco-papiyon/irisync/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) != 0 && args[0] == "scan-remote" {
		return runRemoteScan(args[1:], os.Stdout, stderr)
	}
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage of controller:")
		flags.PrintDefaults()
		fmt.Fprintln(stderr, "  controller scan-remote --config <path> --pair <id>")
	}
	var configPath string
	var hasConfig bool
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
	if hasConfig {
		if _, err := config.Load(configPath); err != nil {
			fmt.Fprintln(stderr, "controller:", err)
			return 1
		}
	}
	return 0
}
