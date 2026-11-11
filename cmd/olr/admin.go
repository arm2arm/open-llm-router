package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const adminUsage = `usage:
  olr admin user add -username NAME [-email EMAIL] [-groups staff,admins]
  olr admin user disable -id USER_UUID
  olr admin key create -user USER_UUID [-label LABEL] [-lifetime 2160h] [-config FILE]
  olr admin key list -user USER_UUID
  olr admin key revoke -id KEY_UUID

OLR_DATABASE_URL is required. User add prints the new UUID. Key create prints
the plaintext key once; save it securely. Key list prints metadata as JSON.
Default key lifetime: auth.default_key_lifetime_days in -config/$OLR_CONFIG,
or 90 days without a config file. Revokes and disables invalidate router caches.
`

func admin(args []string, out, errout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := fmt.Fprint(out, adminUsage)
		return err
	}
	if len(args) < 2 {
		fmt.Fprint(errout, adminUsage)
		return errUsage
	}
	command := args[0] + " " + args[1]
	fs := flag.NewFlagSet("olr admin "+command, flag.ContinueOnError)
	fs.SetOutput(errout)
	var username, email, groups, userID, id, label, configPath string
	var lifetime time.Duration
	switch command {
	case "user add":
		fs.StringVar(&username, "username", "", "local username (required)")
		fs.StringVar(&email, "email", "", "email attribute")
		fs.StringVar(&groups, "groups", "", "comma-separated group names")
	case "user disable", "key revoke":
		fs.StringVar(&id, "id", "", "record UUID (required)")
	case "key create":
		fs.StringVar(&userID, "user", "", "owner's UUID (required)")
		fs.StringVar(&label, "label", "", "display label")
		fs.DurationVar(&lifetime, "lifetime", 0, "key lifetime, e.g. 720h (default from config, or 2160h)")
		fs.StringVar(&configPath, "config", os.Getenv("OLR_CONFIG"), "YAML config file (default: $OLR_CONFIG)")
	case "key list":
		fs.StringVar(&userID, "user", "", "owner's UUID (required)")
	default:
		fmt.Fprint(errout, adminUsage)
		return errUsage
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments; use named flags shown by -h")
	}
	if command == "user add" && strings.TrimSpace(username) == "" {
		return errors.New("-username is required")
	}
	if command == "key create" || command == "key list" {
		if !validUUID(userID) {
			return errors.New("-user must be a user UUID")
		}
	}
	if command == "key revoke" || command == "user disable" {
		if !validUUID(id) {
			return errors.New("-id must be a record UUID")
		}
	}
	if command == "key create" {
		days := config.DefaultKeyLifetimeDays
		if configPath != "" {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			days = cfg.Auth.DefaultKeyLifetimeDays
		}
		explicit := false
		fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "lifetime" })
		if !explicit {
			lifetime = time.Duration(days) * 24 * time.Hour
		}
		if lifetime < time.Microsecond {
			return errors.New("-lifetime must be at least 1us")
		}
	}
	var groupNames []string
	if groups != "" {
		for group := range strings.SplitSeq(groups, ",") {
			group = strings.TrimSpace(group)
			if group == "" {
				return errors.New("-groups contains an empty group name")
			}
			groupNames = append(groupNames, group)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	manager := auth.Admin{Pool: pool}
	switch command {
	case "user add":
		id, err := manager.AddUser(ctx, username, email, groupNames)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, id)
		return err
	case "user disable":
		return manager.DisableUser(ctx, id)
	case "key create":
		key, err := manager.CreateKey(ctx, userID, label, lifetime)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, key)
		return err
	case "key list":
		keys, err := manager.ListKeys(ctx, userID)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(keys)
	case "key revoke":
		return manager.RevokeKey(ctx, id)
	}
	return nil
}

func validUUID(text string) bool {
	var id pgtype.UUID
	return id.Scan(text) == nil && id.Valid
}
