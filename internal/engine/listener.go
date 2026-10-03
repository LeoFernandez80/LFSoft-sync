package engine

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

type Listener struct {
	pool   *pgxpool.Pool
	notify chan string
	log    *zap.SugaredLogger
}

func NewListener(pool *pgxpool.Pool, log *zap.SugaredLogger) *Listener {
	return &Listener{
		pool:   pool,
		notify: make(chan string, 100),
		log:    log,
	}
}

func (l *Listener) C() <-chan string {
	return l.notify
}

func (l *Listener) Run(ctx context.Context) {
	for {
		if err := l.listen(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			l.log.Warnw("listener reconnecting", "err", err)
		}
	}
}

func (l *Listener) listen(ctx context.Context) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, "LISTEN sync_change")
	if err != nil {
		return err
	}
	l.log.Info("LISTEN sync_change active")

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		// payload format: "entity:row_id"
		parts := strings.SplitN(notification.Payload, ":", 2)
		if len(parts) >= 1 {
			select {
			case l.notify <- parts[0]:
			default:
				l.log.Warnw("listener channel full, dropping", "entity", parts[0])
			}
		}
	}
}
