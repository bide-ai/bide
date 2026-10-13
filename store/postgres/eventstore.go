package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// eventsPage is how many leaves a Load reads per query. No connection is held between pages.
const eventsPage = 256

// EventStore is a Postgres-backed audit.EventStore: the durable copy of a run's event trail (see
// audit.PersistJournal and audit.LoadEventLog), in the bide_events table (see WithTablePrefix and
// WithSchema). It shares the Store's pool, so Close on the Store closes it, and it needs no setup
// beyond Open or New, which create the table.
//
// A leaf is a row with primary key (run_id, seq), and Append is one INSERT, committed before it
// returns, that inserts the leaf only at the run's next position with ON CONFLICT DO NOTHING on
// that key. Writers racing for a position, from any connection or node, all run the statement:
// Postgres lets one insert, makes the others wait for it, and then they find its row, report a
// no-op for the same bytes and audit.ErrSeqTaken for different ones. A statement that fails with
// a serialization failure or a deadlock is run again, as the Store's are. A crash leaves the trail
// as of the last committed Append: a leaf is never partly written, and a re-mirror
// (audit.PersistJournal) appends what is missing. Leaves are stored as the exact bytes given.
//
// The table is plain: the store never updates or deletes a leaf, so a deployment that wants the
// trail insert-only can grant its role INSERT and SELECT on bide_events alone. A store opened with
// OpenReadOnly does not create the table and can read it only if it exists.
type EventStore struct{ s *Store }

var _ audit.EventStore = (*EventStore)(nil)

// EventStore returns the audit.EventStore over s's database. It is cheap; the handle holds no
// state of its own.
func (s *Store) EventStore() *EventStore { return &EventStore{s: s} }

// Append implements audit.EventStore. A seq past the run's next position, or a negative one, is
// an error; a different leaf at a stored seq fails with an error that wraps audit.ErrSeqTaken.
func (e *EventStore) Append(ctx context.Context, runID string, seq int, leaf []byte) error {
	if seq < 0 {
		return fmt.Errorf("postgres: event position %d of run %s is negative", seq, runID)
	}
	s := e.s
	n, err := s.write(ctx, s.t.appendEvent, runID, int64(seq), leaf)
	if err != nil {
		return fmt.Errorf("postgres: append event %d of run %s: %w (%w)", seq, runID, err, agent.ErrStorage)
	}
	if n == 1 {
		return nil
	}
	// Nothing inserted: the position is taken, or beyond the next one. ON CONFLICT does nothing
	// only once the row's writer has committed, so a new statement sees the row.
	stored, err := retry(ctx, func() ([]byte, error) {
		var b []byte
		err := s.db.QueryRowContext(ctx, string(s.t.getEvent), runID, int64(seq)).Scan(&b)
		return b, err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: event %d of run %s is non-contiguous", seq, runID)
	}
	if err != nil {
		return fmt.Errorf("postgres: read event %d of run %s: %w (%w)", seq, runID, err, agent.ErrStorage)
	}
	if !bytes.Equal(stored, leaf) {
		return fmt.Errorf("postgres: event %d of run %s already stored with different bytes (fork/tamper): %w", seq, runID, audit.ErrSeqTaken)
	}
	return nil
}

// Load implements audit.EventStore. It reads eventsPage leaves per query, so it holds no
// connection between pages; the trail only grows, so the pages join into one contiguous trail.
func (e *EventStore) Load(ctx context.Context, runID string) ([][]byte, error) {
	var out [][]byte
	for {
		page, err := retry(ctx, func() ([][]byte, error) { return e.loadPage(ctx, runID, len(out)) })
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < eventsPage {
			return out, nil
		}
	}
}

func (e *EventStore) loadPage(ctx context.Context, runID string, from int) ([][]byte, error) {
	rows, err := e.s.db.QueryContext(ctx, string(e.s.t.loadEvents), runID, int64(from), eventsPage)
	if err != nil {
		return nil, fmt.Errorf("postgres: load events of run %s: %w (%w)", runID, err, agent.ErrStorage)
	}
	defer rows.Close()
	var page [][]byte
	for rows.Next() {
		var seq int64
		var leaf []byte
		if err := rows.Scan(&seq, &leaf); err != nil {
			return nil, fmt.Errorf("postgres: scan event: %w (%w)", err, agent.ErrStorage)
		}
		if seq != int64(from+len(page)) {
			return nil, fmt.Errorf("postgres: events of run %s are not contiguous at %d (found %d): %w", runID, from+len(page), seq, agent.ErrStorage)
		}
		page = append(page, leaf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: load events of run %s: %w (%w)", runID, err, agent.ErrStorage)
	}
	return page, nil
}
