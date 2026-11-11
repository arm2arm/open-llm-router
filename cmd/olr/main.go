// Command olr is the open-llm-router: one binary that serves the API and,
// the bootstrap admin commands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/home"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/metrics"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/retention"
	"github.com/arm2arm/open-llm-router/internal/server"
	usagewriter "github.com/arm2arm/open-llm-router/internal/usage"
)

const usage = `usage: olr <command> [flags]

commands:
  serve    run the router
  admin    manage local users and API keys
  migrate  apply database migrations and exit

Run "olr <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(args)
	case "admin":
		err = admin(args, os.Stdout, os.Stderr)
	case "migrate":
		err = migrateCommand(args, os.Stdout)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "olr: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		// Start-up errors go to stderr as plain text: they are read by the
		// person who started the process. Everything after that is a JSON
		// log line on stdout.
		fmt.Fprintf(os.Stderr, "olr: %v\n", err)
		os.Exit(1)
	}
}

// errUsage reports a command line that the flag package has already
// explained on stderr.
var errUsage = errors.New("invalid command line")

func serve(args []string) error {
	fs := flag.NewFlagSet("olr serve", flag.ContinueOnError)
	configPath := fs.String("config", os.Getenv("OLR_CONFIG"), "path of the YAML config `file` (default: $OLR_CONFIG)")
	listen := fs.String("listen", ":8080", "`address` to listen on")
	grace := fs.Duration("shutdown-grace", 30*time.Second, "`time` that requests in flight get to finish after SIGTERM")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "olr serve: unexpected argument %q\n", fs.Arg(0))
		return errUsage
	}
	if *configPath == "" {
		return errors.New("no config file: set OLR_CONFIG or pass -config")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// The first SIGTERM or Ctrl-C starts a graceful shutdown. Handling of the
	// signal is then switched off again, so that a second one ends the
	// process at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	startupCtx, cancelStartup := context.WithTimeout(ctx, 10*time.Second)
	database, err := db.Open(startupCtx, cfg.DatabaseURL)
	cancelStartup()
	if err != nil {
		return err
	}
	defer database.Close()
	authCtx, cancelAuth := context.WithTimeout(ctx, 10*time.Second)
	authenticator, err := auth.New(authCtx, database)
	cancelAuth()
	if err != nil {
		return err
	}
	defer authenticator.Close()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer ln.Close()

	accounting := usagewriter.NewWriterInLocation(database, log, cfg.Limits.Location)
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := accounting.Close(flushCtx); err != nil {
			log.Error("usage shutdown flush incomplete")
		}
	}()
	forwarder := proxy.New(cfg)
	forwarder.Usage = accounting
	forwarder.Limits = limits.New(database, cfg.Limits.Location, time.Now)
	observabilityOptions, err := metrics.Environment(os.Getenv)
	if err != nil {
		return err
	}
	observability, err := metrics.New(ctx, observabilityOptions, forwarder.Pools, accounting)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observability.Shutdown(flushCtx); err != nil {
			log.Error("trace shutdown flush incomplete")
		}
	}()
	forwarder.Metrics = observability
	defer forwarder.Close()
	retentionDone := make(chan struct{})
	retentionCtx, cancelRetention := context.WithCancel(context.Background())
	go func() {
		defer close(retentionDone)
		retention.Run(retentionCtx, database, cfg.Retention, cfg.Limits.Location, log)
	}()
	defer func() { cancelRetention(); <-retentionDone }()
	forwarder.Pools.Check(ctx)
	// Health checks keep running during graceful shutdown until Run returns.
	healthCtx, cancelHealth := context.WithCancel(context.Background())
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); forwarder.Pools.Run(healthCtx) }()
	defer func() { cancelHealth(); <-healthDone }()
	srv := server.New(log)
	srv.Handle("/", home.New(cfg.OIDC != nil))
	srv.SetAPIMiddleware(authenticator.Middleware)
	srv.SetReadiness(func(ctx context.Context) bool {
		return forwarder.Pools.Ready() && database.Ping(ctx) == nil
	})
	forwarder.Register(srv.Handle)
	srv.Handle("GET /metrics", observability.Handler())
	if cfg.OIDC != nil {
		portalCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		portalHandler, err := portal.New(portalCtx, cfg, database, forwarder.Limits)
		cancel()
		if err != nil {
			return err
		}
		defer portalHandler.Close()
		srv.Handle("/portal", portalHandler)
		srv.Handle("/portal/", portalHandler)
	}

	log.Info("listening", "addr", ln.Addr().String(), "config", *configPath, "models", len(cfg.Models))
	if err := srv.Run(ctx, ln, *grace); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// migrateCommand applies pending migrations, for deployments that migrate
// before starting routers (a Kubernetes init container or job).
func migrateCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("olr migrate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		return errUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv(config.EnvDatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	version, err := db.Version(ctx, pool)
	if err != nil {
		return errors.New("read schema version failed")
	}
	_, err = fmt.Fprintf(out, "schema version %d\n", version)
	return err
}
