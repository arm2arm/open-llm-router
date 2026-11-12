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
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
)

const recoverAction = "local_admin.recover"

// recoverCommand resets the protected local administrator's password from the
// host or container, using only the database. It never reads the bootstrap
// password sources and has no flag that names an account: it can only replace
// the protected administrator's credential, never delete, disable, rename or
// convert it. Every attempt with a valid command line emits an audit event.
func recoverCommand(args []string, stdin io.Reader, out, errout io.Writer) error {
	fs := flag.NewFlagSet("olr admin recover", flag.ContinueOnError)
	fs.SetOutput(errout)
	passwordFile := fs.String("password-file", "", "private file containing the temporary password")
	fromStdin := fs.Bool("password-stdin", false, "read the temporary password from redirected or piped standard input")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments; pass the password with -password-file or -password-stdin")
	}
	if (*passwordFile == "") == !*fromStdin {
		return errors.New("use exactly one of -password-file FILE or -password-stdin")
	}
	event := audit.Event{Action: recoverAction, Actor: audit.OperatorCLI()}
	record := func(result auth.Recovery, err error, reason string) error {
		event.TargetID = result.ID
		switch {
		case err == nil:
			event.Result = audit.Succeeded
			event.Details = map[string]int64{"credential_revision": result.CredentialRevision,
				"sessions_revoked": result.SessionsRevoked, "keys_revoked": result.KeysRevoked}
		case errors.Is(err, auth.ErrPasswordPolicy), errors.Is(err, auth.ErrPasswordReuse), errors.Is(err, auth.ErrNotBootstrapped):
			event.Result, event.Reason = audit.Refused, err.Error()
		default:
			event.Result, event.Reason = audit.Failed, reason
		}
		audit.Write(errout, event)
		return err
	}

	var password []byte
	var err error
	if *fromStdin {
		password, err = readPasswordStdin(stdin)
	} else {
		password, err = readPasswordFile(*passwordFile)
	}
	if err != nil {
		return record(auth.Recovery{}, err, err.Error())
	}
	defer clear(password)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return record(auth.Recovery{}, err, "database connection failed")
	}
	defer pool.Close()
	result, err := auth.Admin{Pool: pool}.RecoverProtectedAdmin(ctx, password)
	if err != nil {
		return record(result, err, err.Error())
	}
	_ = record(result, nil, "")
	return json.NewEncoder(out).Encode(result)
}

// readPasswordStdin refuses terminals and other devices: typed input would be
// echoed, and only redirected files and pipes are protected input here.
func readPasswordStdin(stdin io.Reader) ([]byte, error) {
	if f, ok := stdin.(*os.File); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return nil, errors.New("-password-stdin needs redirected or piped input, not a terminal; for example: olr admin recover -password-stdin < FILE")
		}
	}
	return readBoundedPassword(stdin)
}
