package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const invalidationChannel = "olr_auth_invalidate"
const listenerName = "olr-auth-cache"
const ackPrefix = "olr_auth_ack_"

// New establishes cache invalidation before returning. Its context bounds
// startup only; Close stops the dedicated listener, including reconnects.
func New(ctx context.Context, pool *pgxpool.Pool) (*Authenticator, error) {
	a := &Authenticator{store: postgresStore{pool}, pool: pool, now: time.Now, done: make(chan struct{}),
		Forwarders: config.Forwarders{HeaderMap: map[string]string{config.EndUserHeader: "username"}, UnknownUser: config.UnknownUserReject}}
	a.invalidate(false)
	conn, err := connectListener(ctx, pool)
	if err != nil {
		return nil, errors.New("start authentication invalidation listener failed")
	}
	a.invalidate(true)
	runCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.listen(runCtx, pool, conn)
	return a, nil
}

func (a *Authenticator) Close() {
	a.cancel()
	<-a.done
}

func connectListener(ctx context.Context, pool *pgxpool.Pool) (*pgx.Conn, error) {
	cfg := pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = ""
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err = conn.Exec(ctx, "LISTEN "+invalidationChannel); err == nil {
		// Advertise only after LISTEN has committed, and before enabling the
		// cache. Admin commands discover listeners through pg_stat_activity.
		_, err = conn.Exec(ctx, "SELECT set_config('application_name', $1, false)", listenerName)
	}
	if err != nil {
		closeConn(conn)
		return nil, err
	}
	return conn, nil
}

func closeConn(conn *pgx.Conn) {
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

func (a *Authenticator) listen(ctx context.Context, pool *pgxpool.Pool, conn *pgx.Conn) {
	defer close(a.done)
	defer func() { closeConn(conn) }()
	for ctx.Err() == nil {
		n, err := conn.WaitForNotification(ctx)
		if err == nil {
			a.invalidate(true)
			if strings.HasPrefix(n.Payload, ackPrefix) && len(n.Payload) <= 63 {
				ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_, err = conn.Exec(ackCtx, "SELECT pg_notify($1, $2)", n.Payload, strconv.FormatUint(uint64(conn.PgConn().PID()), 10))
				cancel()
			}
			if err == nil {
				continue
			}
		}
		// Never serve cached credentials while the invalidation connection is
		// known to be down. SQL lookups remain available during reconnects.
		a.invalidate(false)
		closeConn(conn)
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			conn, err = connectListener(connectCtx, pool)
			cancel()
			if err == nil {
				a.invalidate(true)
				break
			}
		}
	}
}

// change publishes invalidation in the same transaction as the mutation, then
// waits for every advertised router to clear its cache. Plain NOTIFY alone would
// leave a scheduling race between the CLI returning and the next HTTP request.
// Acknowledgements contain process IDs only, never key material or user data.
func (a Admin) change(ctx context.Context, sql string, args ...any) error {
	return a.changeWith(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return errors.New("update authentication record failed")
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (a Admin) changeWith(ctx context.Context, mutate func(pgx.Tx) error) error {
	conn, err := pgx.ConnectConfig(ctx, a.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return errors.New("connect for authentication change failed")
	}
	defer closeConn(conn)
	channel := ackPrefix + strings.ToLower(rand.Text())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return errors.New("listen for cache invalidation acknowledgements failed")
	}
	pending := map[string]bool{}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := mutate(tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT pid::text FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = $1`, listenerName)
		if err != nil {
			return errors.New("discover authentication listeners failed")
		}
		for rows.Next() {
			var pid string
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return errors.New("read authentication listener failed")
			}
			pending[pid] = true
		}
		rows.Close()
		if rows.Err() != nil {
			return errors.New("discover authentication listeners failed")
		}
		if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", invalidationChannel, channel); err != nil {
			return errors.New("publish cache invalidation failed")
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return errors.New("authentication change transaction failed")
	}
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for len(pending) > 0 {
		n, err := conn.WaitForNotification(ackCtx)
		if err != nil {
			return errors.New("change committed but cache invalidation was not acknowledged; retry the command")
		}
		delete(pending, n.Payload)
	}
	return nil
}
