package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/arm2arm/open-llm-router/internal/config"
)

const configUsage = `usage:
  olr config check [-config FILE]

Check validates a config file with the same strict parser the router uses:
unknown keys, missing ${VAR} or bearer_token_env variables and invalid values
are reported together. It reads only the file and the environment: no
database, migration or network access. Exit status 0 means valid.
`

func configCommand(args []string, out, errout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := fmt.Fprint(out, configUsage)
		return err
	}
	if args[0] != "check" {
		fmt.Fprint(errout, configUsage)
		return errUsage
	}
	fs := flag.NewFlagSet("olr config check", flag.ContinueOnError)
	fs.SetOutput(errout)
	path := fs.String("config", os.Getenv("OLR_CONFIG"), "YAML config file (default: $OLR_CONFIG)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments; use -config FILE")
	}
	if *path == "" {
		return errors.New("no config file: set OLR_CONFIG or pass -config")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	sso := cfg.OIDC != nil
	return json.NewEncoder(out).Encode(map[string]any{"valid": true, "models": len(cfg.Models), "aliases": len(cfg.Aliases),
		"groups": len(cfg.Groups), "sso": sso})
}
