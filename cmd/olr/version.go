package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/arm2arm/open-llm-router/internal/buildinfo"
)

func versionCommand(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(out)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("version: unexpected arguments")
	}
	_, err := fmt.Fprintf(out, "olr %s\ncommit %s\nGo version: %s\n", buildinfo.Version, buildinfo.Commit, buildinfo.GoVersion())
	return err
}
