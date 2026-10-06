package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// notifyChannel carries a notice whenever a job's status, items or log
// change. The payload is the job's ID, followed by logSuffix for a log line
// or cancelSuffix for a cancel request.
const notifyChannel = "battleship_events"

// logSuffix marks a notice of a log line, and cancelSuffix one of a cancel
// request.
const (
	logSuffix    = " log"
	cancelSuffix = " cancel"
)

// Notice says that a job changed. Log marks a log line: AddEvent adds those
// to running jobs only, and the step they set on an item, pending or
// running, leaves it busy, so only the job's own page shows the change.
// Every other notice may change the job's status, which job lists and busy
// VMs show. Cancel marks a request to cancel the job, which the worker
// running it acts on.
type Notice struct {
	JobID  int64
	Log    bool
	Cancel bool
}

// notify sends a notice that the job's status or items changed, when tx
// commits.
func notify(ctx context.Context, tx pgx.Tx, jobID int64) error {
	return send(ctx, tx, strconv.FormatInt(jobID, 10))
}

// notifyLog sends a notice that the job logged a line, when tx commits.
func notifyLog(ctx context.Context, tx pgx.Tx, jobID int64) error {
	return send(ctx, tx, strconv.FormatInt(jobID, 10)+logSuffix)
}

// notifyCancel sends a notice that someone asked to cancel the job, when tx
// commits.
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

// Notifications delivers a notice for each change to a job, using Postgres
// LISTEN, until ctx is done. It holds one database connection. The channel
// is closed when ctx is done or the connection fails; callers that need to
// keep listening subscribe again, and should re-read state they care about,
// since changes in between aren't delivered.
//
// A process should listen once and fan out: in battleship serve, status.Hub
// is the only caller, and pages subscribe to the hub.
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
