package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxListener wraps a dedicated pgx connection for LISTEN/NOTIFY.
// database/sql does not support LISTEN — a raw pgx pool is required.
type OutboxListener struct {
	pool *pgxpool.Pool
}

// NewOutboxListener creates a listener with a single-connection pool
// dedicated to receiving outbox_new notifications.
func NewOutboxListener(dsn string) (*OutboxListener, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 1

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &OutboxListener{pool: pool}, nil
}

// Listen blocks until a notification arrives on the "outbox_new" channel
// or ctx is cancelled. Returns the outbox message ID from the payload.
func (l *OutboxListener) Listen(ctx context.Context) (string, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN outbox_new"); err != nil {
		return "", err
	}

	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		return "", err
	}
	return n.Payload, nil
}

// Close shuts down the listener pool.
func (l *OutboxListener) Close() {
	l.pool.Close()
}

// Healthy reports whether the listener's dedicated connection pool is
// reachable. Used by the /readyz endpoint to distinguish "process up"
// from "ready to serve traffic". Returns an error describing the
// failing subsystem when unreachable.
func (l *OutboxListener) Healthy(ctx context.Context) error {
	if l == nil || l.pool == nil {
		return errors.New("outbox listener not initialised")
	}
	return l.pool.Ping(ctx)
}
