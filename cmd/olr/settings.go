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

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/settings"
)

const settingsUsage = `usage:
  olr admin settings show     [-domain authentication|routing] [-config FILE]
  olr admin settings import   [-domain authentication|routing] [-config FILE]
  olr admin settings draft    -expect-version N -document FILE [-secret NAME=FILE]... [-domain ...] [-config FILE]
  olr admin settings discard  -expect-version N [-domain ...] [-config FILE]
  olr admin settings activate -expect-version N -revision ID [-domain ...] [-config FILE]
  olr admin settings rollback -expect-version N -revision ID [-domain ...] [-config FILE]

Managed settings move a domain from the config file and environment into
PostgreSQL revisions. Domains: authentication (the SSO connection, default)
and routing (logical models with endpoint pools, aliases and group policies). Import is explicit and keeps the current behavior; after
it, the active revision is the only authority for the domain. Each command
prints the domain view as JSON: effective values and sources, active and draft
revisions with their checks, conflicts and history. Secrets are shown as
configured/available states, never values.

-expect-version is the "version" from the last show; a concurrent change makes
the command fail instead of overwriting it. Draft documents are JSON in the
shape of "document" in show. -secret stores a new encrypted value for a
secret setting (portal_client_secret, sync_client_secret) from a private file
and requires OLR_SETTINGS_ENCRYPTION_KEY. Activation rechecks the draft and
refuses it while a file or environment value contradicts it. Routers apply the
active revision when they next start. -config defaults to $OLR_CONFIG.
`

type secretFlags map[string]string

func (s secretFlags) String() string { return "" }
func (s secretFlags) Set(value string) error {
	name, file, ok := strings.Cut(value, "=")
	if !ok || name == "" || file == "" {
		return errors.New("use NAME=FILE")
	}
	if _, dup := s[name]; dup {
		return errors.New("each secret may be given once")
	}
	s[name] = file
	return nil
}

func settingsCommand(args []string, out, errout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := fmt.Fprint(out, settingsUsage)
		return err
	}
	command := args[0]
	fs := flag.NewFlagSet("olr admin settings "+command, flag.ContinueOnError)
	fs.SetOutput(errout)
	domain := fs.String("domain", settings.DomainAuthentication, "settings domain")
	configPath := fs.String("config", os.Getenv("OLR_CONFIG"), "YAML config file (default: $OLR_CONFIG)")
	var expected, revision int64
	var documentPath string
	secretFiles := secretFlags{}
	switch command {
	case "show", "import":
	case "draft":
		fs.Int64Var(&expected, "expect-version", 0, "version from the last show (required)")
		fs.StringVar(&documentPath, "document", "", "JSON settings document (required)")
		fs.Var(secretFiles, "secret", "NAME=FILE: store a new secret value from a private file (repeatable)")
	case "discard":
		fs.Int64Var(&expected, "expect-version", 0, "version from the last show (required)")
	case "activate", "rollback":
		fs.Int64Var(&expected, "expect-version", 0, "version from the last show (required)")
		fs.Int64Var(&revision, "revision", 0, "revision ID (required)")
	default:
		fmt.Fprint(errout, settingsUsage)
		return errUsage
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments; use named flags shown by -h")
	}
	if *domain != settings.DomainAuthentication && *domain != settings.DomainRouting {
		return settings.ErrUnknownDomain
	}
	if command != "show" && command != "import" && expected <= 0 {
		return errors.New("-expect-version must be the positive version from olr admin settings show")
	}
	if (command == "activate" || command == "rollback") && revision <= 0 {
		return errors.New("-revision must be a revision ID from olr admin settings show")
	}
	if command == "draft" && documentPath == "" {
		return errors.New("-document is required")
	}
	if *configPath == "" {
		return errors.New("no config file: set OLR_CONFIG or pass -config")
	}
	file, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	var document []byte
	if command == "draft" {
		if document, err = readDocument(documentPath); err != nil {
			return err
		}
	}
	secretValues := map[string][]byte{}
	defer func() {
		for _, value := range secretValues {
			clear(value)
		}
	}()
	for name, path := range secretFiles {
		value, err := readPasswordFile(path)
		if err != nil {
			return fmt.Errorf("-secret %s: %w", name, err)
		}
		secretValues[name] = value
	}
	cipher, err := settings.LoadCipher(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	request := settingsRequest{command: command, domain: *domain, expected: expected, revision: revision, document: document, secrets: secretValues, actor: audit.OperatorCLI()}
	var active, draft int64
	var draftValid bool
	if *domain == settings.DomainRouting {
		// The import baseline is the routing in use, including Admin endpoint
		// revisions; credentials resolve through the backend token policy.
		destinations, err := backendpolicy.New(file, os.LookupEnv)
		if err != nil {
			return err
		}
		current, err := endpoints.Load(ctx, pool, file, destinations)
		if err != nil {
			return err
		}
		service := &settings.RoutingService{DB: pool, File: file, Cipher: cipher, Tokens: destinations.Token,
			Current: func() *config.Config { return current }, Verify: endpoints.VerifyDestinations(destinations)}
		view, err := runSettings[settings.RoutingView](ctx, service, request)
		if err != nil {
			return err
		}
		active, draft, draftValid = view.Summary()
		err = printView(out, view)
		if err != nil {
			return err
		}
	} else {
		service := &settings.AuthenticationService{DB: pool, File: file, Cipher: cipher}
		view, err := runSettings[settings.AuthenticationView](ctx, service, request)
		if err != nil {
			return err
		}
		active, draft, draftValid = view.Summary()
		if err := printView(out, view); err != nil {
			return err
		}
	}
	switch command {
	case "import", "activate", "rollback":
		fmt.Fprintf(errout, "olr: revision %d is active; routers apply it when they next start or on Apply in Admin\n", active)
	case "draft":
		if draft != 0 && !draftValid {
			fmt.Fprintf(errout, "olr: draft %d was saved but cannot be activated yet; see draft.validation.problems\n", draft)
		}
	}
	return nil
}

type settingsRequest struct {
	command, domain    string
	expected, revision int64
	document           []byte
	secrets            map[string][]byte
	actor              audit.Actor
}

// settingsOps is what every managed settings service offers.
type settingsOps[V any] interface {
	View(context.Context, string) (V, error)
	Import(context.Context, string, audit.Actor) (V, error)
	SaveDraft(context.Context, string, int64, []byte, map[string][]byte, audit.Actor) (V, error)
	Discard(context.Context, string, int64, audit.Actor) (V, error)
	Activate(context.Context, string, int64, int64, audit.Actor, bool) (V, error)
	Rollback(context.Context, string, int64, int64, audit.Actor) (V, error)
}

// runSettings performs one command. CLI activation is an operator path and
// does not require the Admin page's stored checks.
func runSettings[V any](ctx context.Context, ops settingsOps[V], r settingsRequest) (V, error) {
	switch r.command {
	case "import":
		return ops.Import(ctx, r.domain, r.actor)
	case "draft":
		return ops.SaveDraft(ctx, r.domain, r.expected, r.document, r.secrets, r.actor)
	case "discard":
		return ops.Discard(ctx, r.domain, r.expected, r.actor)
	case "activate":
		return ops.Activate(ctx, r.domain, r.expected, r.revision, r.actor, false)
	case "rollback":
		return ops.Rollback(ctx, r.domain, r.expected, r.revision, r.actor)
	default:
		return ops.View(ctx, r.domain)
	}
}

func printView(out io.Writer, view any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(view)
}

func readDocument(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("settings document could not be opened")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return nil, errors.New("settings document could not be read or exceeds 64 KiB")
	}
	return data, nil
}
