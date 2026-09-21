package db

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listen holds a dedicated connection that LISTENs on the given channels and calls handle for every notification.
// It reconnects with backoff when the connection is lost, and blocks until ctx ends.
//
// NOTIFY is fire-and-forget, so anything sent while the connection was down is gone. onReady is called after every
// successful (re)connect with reconnected=true for all but the first, so callers can refetch what they missed.
func Listen(ctx context.Context, dsn string, channels []string, onReady func(reconnected bool), handle func(channel, payload string)) {
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		err := listenOnce(ctx, dsn, channels, func() {
			backoff = time.Second
			onReady(!first)
			first = false
		}, handle)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("listener lost, reconnecting", "channels", channels, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func listenOnce(ctx context.Context, dsn string, channels []string, ready func(), handle func(channel, payload string)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return err
		}
	}
	ready()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		handle(n.Channel, n.Payload)
	}
}
