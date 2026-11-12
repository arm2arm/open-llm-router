package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/retention"
	"github.com/jackc/pgx/v5/pgtype"
)

const adminUsage = `usage:
  olr admin bootstrap [-password-file FILE]
  olr admin password change -user USER_UUID -current-password-file FILE -password-file FILE
  olr admin user add -username NAME [-email EMAIL] [-groups staff,admins]
  olr admin user disable -id USER_UUID
  olr admin user delete -id USER_UUID
  olr admin user list [-match TEXT]
  olr admin key create -user USER_UUID [-label LABEL] [-class interactive|batch] [-trusted-forwarder] [-lifetime 2160h] [-config FILE]
  olr admin key list -user USER_UUID
  olr admin key revoke -id KEY_UUID
  olr admin retention run [-config FILE]
  olr admin override set -user USER_UUID -field FIELD -value N|unlimited [-reason TEXT] [-by NAME]
  olr admin override clear -user USER_UUID -field FIELD
  olr admin override list -user USER_UUID

OLR_DATABASE_URL is required. User add prints the new UUID. Key create prints
the plaintext key once; save it securely. Key and user list print JSON.
Override fields: tokens_per_day, tokens_per_month, rpm, tpm, max_concurrent,
max_concurrent_batch, strict_quota (1 or 0). An override replaces the group
value for one user and applies to the next request without a restart.
Retention run deletes usage older than the retention periods in -config
(defaults: 90 days of records, 400 days of daily totals) and prints counts.
User delete erases the user with keys, sessions, usage records and daily
totals (GDPR erasure); it cannot be undone.
Default key lifetime: auth.default_key_lifetime_days in -config/$OLR_CONFIG,
or 90 days without a config file. Revokes and disables invalidate router caches.
Bootstrap creates the protected local admin once; later calls never reset it.
Use OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE (preferred) or OLR_BOOTSTRAP_ADMIN_PASSWORD.
Password files must be private (0400/0600); passwords need 15+ characters and
at most 1024 bytes. Initial password change is required before session/API-key
access. Password change requires the current password; recovery is separate.
`

func admin(args []string, out, errout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := fmt.Fprint(out, adminUsage)
		return err
	}
	if args[0] == "bootstrap" {
		return bootstrapCommand(args[1:], out, errout)
	}
	if len(args) < 2 {
		fmt.Fprint(errout, adminUsage)
		return errUsage
	}
	command := args[0] + " " + args[1]
	fs := flag.NewFlagSet("olr admin "+command, flag.ContinueOnError)
	fs.SetOutput(errout)
	var username, email, groups, userID, id, label, configPath, match, class, field, value, reason, by string
	var passwordFile, currentPasswordFile string
	var lifetime time.Duration
	var trusted bool
	switch command {
	case "password change":
		fs.StringVar(&userID, "user", "", "local user UUID (required)")
		fs.StringVar(&currentPasswordFile, "current-password-file", "", "private current password file (required)")
		fs.StringVar(&passwordFile, "password-file", "", "private new password file (required)")
	case "user add":
		fs.StringVar(&username, "username", "", "local username (required)")
		fs.StringVar(&email, "email", "", "email attribute")
		fs.StringVar(&groups, "groups", "", "comma-separated group names")
	case "user list":
		fs.StringVar(&match, "match", "", "part of a username or email, ignoring case")
	case "user disable", "user delete", "key revoke":
		fs.StringVar(&id, "id", "", "record UUID (required)")
	case "key create":
		fs.StringVar(&userID, "user", "", "owner's UUID (required)")
		fs.StringVar(&label, "label", "", "display label")
		fs.StringVar(&class, "class", "interactive", "interactive or batch (separate concurrency limits)")
		fs.BoolVar(&trusted, "trusted-forwarder", false, "the key may name end users (forwarders.header_map), e.g. for Open WebUI")
		fs.DurationVar(&lifetime, "lifetime", 0, "key lifetime, e.g. 720h (default from config, or 2160h)")
		fs.StringVar(&configPath, "config", os.Getenv("OLR_CONFIG"), "YAML config file (default: $OLR_CONFIG)")
	case "key list":
		fs.StringVar(&userID, "user", "", "owner's UUID (required)")
	case "override set", "override clear", "override list":
		fs.StringVar(&userID, "user", "", "user UUID (required)")
		if command != "override list" {
			fs.StringVar(&field, "field", "", "limit to override (required)")
		}
		if command == "override set" {
			fs.StringVar(&value, "value", "", "a positive number, or unlimited (required)")
			fs.StringVar(&reason, "reason", "", "why, for the audit trail")
			fs.StringVar(&by, "by", "cli", "who sets it, for the audit trail")
		}
	case "retention run":
		fs.StringVar(&configPath, "config", os.Getenv("OLR_CONFIG"), "YAML config file (default: $OLR_CONFIG)")
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
	if command == "password change" && (!validUUID(userID) || passwordFile == "" || currentPasswordFile == "") {
		return errors.New("-user UUID, -current-password-file and -password-file are required")
	}
	if command == "user add" && strings.TrimSpace(username) == "" {
		return errors.New("-username is required")
	}
	var overrideValue *int64
	if strings.HasPrefix(command, "override ") {
		if !validUUID(userID) {
			return errors.New("-user must be a user UUID")
		}
		if command != "override list" && !slices.Contains(policy.OverrideFields, field) {
			return fmt.Errorf("-field must be one of %s", strings.Join(policy.OverrideFields, ", "))
		}
		if command == "override set" {
			if value != "unlimited" {
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return errors.New("-value must be a number or unlimited")
				}
				overrideValue = &n
			}
			if !policy.ValidOverride(field, overrideValue) {
				return errors.New("-value is not allowed for this field (positive; strict_quota takes 1 or 0)")
			}
		}
	}
	if command == "key create" || command == "key list" {
		if !validUUID(userID) {
			return errors.New("-user must be a user UUID")
		}
	}
	if command == "key revoke" || command == "user disable" || command == "user delete" {
		if !validUUID(id) {
			return errors.New("-id must be a record UUID")
		}
	}
	if command == "key create" {
		if class != "interactive" && class != "batch" {
			return errors.New("-class must be interactive or batch")
		}
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
	periods, location := config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays}, time.UTC
	if command == "retention run" && configPath != "" {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		periods, location = cfg.Retention, cfg.Limits.Location
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
	timeout := 15 * time.Second
	if command == "retention run" {
		timeout = 30 * time.Minute // a first run may delete a large backlog
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	manager := auth.Admin{Pool: pool}
	switch command {
	case "password change":
		current, err := readPasswordFile(currentPasswordFile)
		if err != nil {
			return err
		}
		defer clear(current)
		next, err := readPasswordFile(passwordFile)
		if err != nil {
			return err
		}
		defer clear(next)
		return manager.ChangeLocalPassword(ctx, userID, current, next)
	case "user add":
		id, err := manager.AddUser(ctx, username, email, groupNames)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, id)
		return err
	case "user disable":
		return manager.DisableUser(ctx, id)
	case "user delete":
		return manager.DeleteUser(ctx, id)
	case "user list":
		users, err := manager.ListUsers(ctx, match)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(users)
	case "key create":
		create := manager.CreateKeyOfClass
		if trusted {
			create = manager.CreateForwarderKey
		}
		key, err := create(ctx, userID, label, class, lifetime)
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
	case "override set":
		return manager.SetOverride(ctx, userID, field, overrideValue, reason, by)
	case "override clear":
		return manager.ClearOverride(ctx, userID, field)
	case "override list":
		list, err := manager.ListOverrides(ctx, userID)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(list)
	case "retention run":
		result, err := retention.Prune(ctx, pool, periods, location, time.Now())
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	}
	return nil
}

func validUUID(text string) bool {
	var id pgtype.UUID
	return id.Scan(text) == nil && id.Valid
}
