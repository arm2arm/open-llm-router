package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
)

func historyCommand(args []string, out, errout io.Writer) error {
	fs := flag.NewFlagSet("olr admin history", flag.ContinueOnError)
	fs.SetOutput(errout)
	filter := audit.Filter{}
	fs.StringVar(&filter.Action, "action", "", "exact action, e.g. credential.reset")
	fs.StringVar(&filter.ActorID, "actor", "", "actor UUID")
	fs.StringVar(&filter.TargetID, "target", "", "target UUID or settings domain")
	fs.StringVar(&filter.Result, "result", "", "succeeded, refused or failed")
	fs.Int64Var(&filter.Before, "before", 0, "next_before cursor from previous page")
	fs.IntVar(&filter.Limit, "limit", 100, "page size, 1 to 200")
	from := fs.String("from", "", "start timestamp (RFC3339)")
	to := fs.String("to", "", "end timestamp (RFC3339)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected history arguments; use named filters")
	}
	for value, target := range map[string]**time.Time{*from: &filter.From, *to: &filter.To} {
		if value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return audit.ErrFilter
			}
			*target = &parsed
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	page, err := audit.List(ctx, pool, filter)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(page)
}
