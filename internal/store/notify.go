package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// notifyChannel's payload is a job ID plus logSuffix or cancelSuffix.
const notifyChannel = "battleship_events"

// Notice payload suffixes.
const (
	logSuffix    = " log"
	cancelSuffix = " cancel"
)

// Notice says that a job changed. A Log notice can't change the job's
// status or busy VMs, so only the job's own page needs it. Cancel marks a
// cancel request for the job's worker.
type Notice struct {
	JobID  int64
	Log    bool
	Cancel bool
}

// notify sends a status/items notice when tx commits.
func notify(ctx context.Context, tx pgx.Tx, jobID int64) error {
	return send(ctx, tx, strconv.FormatInt(jobID, 10))
}

// notifyLog sends a log-line notice when tx commits.
func notifyLog(ctx context.Context, tx pgx.Tx, jobID int64) error {
	return send(ctx, tx, strconv.FormatInt(jobID, 10)+logSuffix)
}

// notifyCancel sends a cancel notice when tx commits.
func notifyCancel(ctx context.Context, tx pgx.Tx, jobID int64) error {
	return send(ctx, tx, strconv.FormatInt(jobID, 10)+cancelSuffix)
}

func send(ctx context.Context, tx pgx.Tx, payload string) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, notifyChannel, payload)
	return err
}

// parseNotice reads a notice's payload.
func parseNotice(payload string) (Notice, error) {
	id, log := strings.CutSuffix(payload, logSuffix)
	id, cancel := strings.CutSuffix(id, cancelSuffix)
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Notice{}, fmt.Errorf("notice %q: %w", payload, err)
	}
	return Notice{JobID: n, Log: log, Cancel: cancel}, nil
}

// Notifications LISTENs for job changes on its own connection. The channel
// closes when ctx is done or the connection fails; re-subscribers must
// re-read state, since changes in between are lost. A process should listen
// once and fan out (status.Hub).
func (s *Store) Notifications(ctx context.Context) (<-chan Notice, error) {
	conn, err := pgx.ConnectConfig(ctx, s.connCfg)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		conn.Close(context.Background()) //nolint:errcheck
		return nil, err
	}
	out := make(chan Notice, 64)
	go func() {
		defer close(out)
		defer conn.Close(context.Background()) //nolint:errcheck
		for {
			n, err := conn.WaitForNotification(ctx)
			if err != nil {
				return
			}
			notice, err := parseNotice(n.Payload)
			if err != nil {
				continue
			}
			select {
			case out <- notice:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
