package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const bootstrapPasswordEnv = "OLR_BOOTSTRAP_ADMIN_PASSWORD"
const bootstrapPasswordFileEnv = "OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE"

func readPasswordFile(path string) ([]byte, error) {
	// A misconfigured FIFO must not hang startup before the regular-file check.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("password file could not be opened")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("password file must be a regular file readable only by its owner")
	}
	password, err := readBoundedPassword(f)
	if err != nil {
		return nil, errors.New("password file could not be read or exceeds the password size limit")
	}
	return password, nil
}

// readBoundedPassword reads at most one maximum-size password plus a line
// ending. One final LF/CRLF from a secret writer is dropped; spaces are kept.
func readBoundedPassword(r io.Reader) ([]byte, error) {
	password, err := io.ReadAll(io.LimitReader(r, 1027))
	if err != nil || len(password) > 1026 {
		clear(password)
		return nil, errors.New("password input could not be read or exceeds the password size limit")
	}
	if bytes.HasSuffix(password, []byte("\n")) {
		password = bytes.TrimSuffix(password, []byte("\n"))
		password = bytes.TrimSuffix(password, []byte("\r"))
	}
	return password, nil
}

func bootstrapAdministrator(ctx context.Context, pool *pgxpool.Pool, passwordFile string, required bool) (auth.LocalAccount, bool, error) {
	admin := auth.Admin{Pool: pool}
	account, err := admin.ProtectedLocalAdmin(ctx)
	if err == nil {
		// An initialized account must not depend on retired bootstrap inputs.
		return account, false, nil
	}
	if !errors.Is(err, auth.ErrNotFound) {
		return auth.LocalAccount{}, false, err
	}
	if passwordFile == "" {
		passwordFile = os.Getenv(bootstrapPasswordFileEnv)
	}
	value := os.Getenv(bootstrapPasswordEnv)
	if passwordFile != "" && value != "" {
		return auth.LocalAccount{}, false, errors.New("configure only one bootstrap administrator password source")
	}
	if passwordFile == "" && value == "" {
		if required {
			return auth.LocalAccount{}, false, errors.New("set OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE or OLR_BOOTSTRAP_ADMIN_PASSWORD to bootstrap the administrator")
		}
		return auth.LocalAccount{}, false, nil
	}
	var password []byte
	if passwordFile != "" {
		password, err = readPasswordFile(passwordFile)
		if err != nil {
			return auth.LocalAccount{}, false, err
		}
	} else {
		password = []byte(value)
	}
	defer clear(password)
	return admin.BootstrapLocalAdmin(ctx, password)
}

func bootstrapCommand(args []string, out, errout io.Writer) error {
	fs := flag.NewFlagSet("olr admin bootstrap", flag.ContinueOnError)
	fs.SetOutput(errout)
	passwordFile := fs.String("password-file", "", "private file containing the one-time password (or $OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments; use -password-file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	account, created, err := bootstrapAdministrator(ctx, pool, *passwordFile, true)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		auth.LocalAccount
		Created bool `json:"created"`
	}{account, created})
}
