package sqlite

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

// EventStore is a SQLite-backed audit.EventStore: the durable copy of a run's event trail (see
// audit.PersistJournal and audit.LoadEventLog), in the file's bide_events table (see
// WithTablePrefix). It shares the Store's connection pools, so Close on the Store closes it, and
// it needs no setup beyond Open or New, which create the table.
//
// A leaf is stored at primary key (run_id, seq), and Append is one statement, committed before it
// returns, that inserts the leaf only at the run's next position and does nothing when the
// position is taken. SQLite runs a write statement under the database's one write lock, so
// writers racing for a position, in this process or in others sharing the file, are serialized:
// one inserts, and the others find its leaf and report a no-op or audit.ErrSeqTaken. A crash
// leaves the trail as of the last committed Append: a leaf is never partly written, and a
// re-mirror (audit.PersistJournal) appends what is missing. Leaves are stored as the exact bytes
// given.
type EventStore struct{ s *Store }

var _ audit.EventStore = (*EventStore)(nil)

// EventStore returns the audit.EventStore over s's database. It is cheap; the handle holds no
// state of its own.
func (s *Store) EventStore() *EventStore { return &EventStore{s: s} }

// Append implements audit.EventStore. A seq past the run's next position, or a negative one, is
// an error; a different leaf at a stored seq fails with an error that wraps audit.ErrSeqTaken.
func (e *EventStore) Append(ctx context.Context, runID string, seq int, leaf []byte) error {
	if seq < 0 {
		return fmt.Errorf("sqlite: event position %d of run %s is negative", seq, runID)
	}
	s := e.s
	res, err := s.write(ctx, s.t.appendEvent, runID, seq, leaf)
	if err != nil {
		return fmt.Errorf("sqlite: append event %d of run %s: %w (%w)", seq, runID, err, agent.ErrStorage)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: append event %d of run %s: %w (%w)", seq, runID, err, agent.ErrStorage)
	}
	if n == 1 {
		return nil
	}
	// Nothing inserted: the position is taken, or beyond the next one. Read on the writer, which
	// has seen every commit.
	var stored []byte
	err = s.w.QueryRowContext(ctx, s.t.getEvent, runID, seq).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: event %d of run %s is non-contiguous", seq, runID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: read event %d of run %s: %w (%w)", seq, runID, err, agent.ErrStorage)
	}
	if !bytes.Equal(stored, leaf) {
		return fmt.Errorf("sqlite: event %d of run %s already stored with different bytes (fork/tamper): %w", seq, runID, audit.ErrSeqTaken)
	}
	return nil
}

// Load implements audit.EventStore. It reads eventsPage leaves per query, so it holds no
// connection between pages; the trail only grows, so the pages join into one contiguous trail.
func (e *EventStore) Load(ctx context.Context, runID string) ([][]byte, error) {
	var out [][]byte
	for {
		page, err := e.loadPage(ctx, runID, len(out))
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
	rows, err := e.s.r.QueryContext(ctx, e.s.t.loadEvents, runID, from, eventsPage)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load events of run %s: %w (%w)", runID, err, agent.ErrStorage)
	}
	defer rows.Close()
	var page [][]byte
	for rows.Next() {
		var seq int
		var leaf []byte
		if err := rows.Scan(&seq, &leaf); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w (%w)", err, agent.ErrStorage)
		}
		if seq != from+len(page) {
			return nil, fmt.Errorf("sqlite: events of run %s are not contiguous at %d (found %d): %w", runID, from+len(page), seq, agent.ErrStorage)
		}
		page = append(page, leaf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load events of run %s: %w (%w)", runID, err, agent.ErrStorage)
	}
	return page, nil
}
