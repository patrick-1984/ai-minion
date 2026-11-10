package store

// Story 7.3: the Worker side of the pipe store (AD-23).
//
// [Store] implements the pipe.Store port of the Solo broker over the tables
// pipe_entries and pipe_outbox of migration 0004. Messages and received turns
// share one commit order (pos), so pipe_read answers them merged and every
// entry has an entry_id that is a valid cursor. The outbox is the durable
// queue of the turns of a shared session that the Hub has not acknowledged.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/pipe"
)

// Entry kinds of pipe_entries.
const (
	// PipeSent is a message that was sent to the session.
	PipeSent = "sent"
	// PipeReceived is a reply of the session's agent.
	PipeReceived = "received"
)

// ErrOutboxFull is returned by [Store.OutboxAdd] when the outbox holds its
// limit of unacknowledged turns.
var ErrOutboxFull = errors.New("store: the turn outbox is full")

var _ pipe.Store = (*Store)(nil)

// PipeEntry is one row of the pipe of a peer, a sent message or a received
// turn, in commit order.
type PipeEntry struct {
	Pos     int64
	Kind    string // PipeSent or PipeReceived
	EntryID string // UUIDv7; the turn_id of a turn, the id of a message
	Peer    pipe.Peer
	Text    string
	At      time.Time
}

// OutboxTurn is a turn that waits for the Hub's acknowledgement.
type OutboxTurn struct {
	TurnID  string
	Session string
	// MessageID is the id the Hub gave the delivery that the turn answers.
	MessageID int64
	Text      string
	At        time.Time
}

func pipeErr(code pipe.Code, format string, args ...any) *pipe.Error {
	return &pipe.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AppendMessage implements [pipe.Store]. The ID of the message is its commit
// position.
func (s *Store) AppendMessage(ctx context.Context, m pipe.Message) (pipe.Message, error) {
	at := m.At
	if at.IsZero() {
		at = s.clock.now()
	}
	id, err := pipe.NewTurnID(at)
	if err != nil {
		return pipe.Message{}, fmt.Errorf("store: append message: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pipe_entries (kind, entry_id, worker_id, session_name, text, at_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		PipeSent, id, m.Peer.WorkerID, m.Peer.SessionName, m.Text, at.UnixMilli())
	if err != nil {
		return pipe.Message{}, fmt.Errorf("store: append message: %w", err)
	}
	pos, err := res.LastInsertId()
	if err != nil {
		return pipe.Message{}, fmt.Errorf("store: append message: %w", err)
	}
	m.ID, m.At = pos, time.UnixMilli(at.UnixMilli()).UTC()
	return m, nil
}

// Messages implements [pipe.Store].
func (s *Store) Messages(ctx context.Context, peer pipe.Peer, afterID int64, limit int) ([]pipe.Message, error) {
	q := `SELECT pos, text, at_ms FROM pipe_entries
	      WHERE kind = ? AND worker_id = ? AND session_name = ? AND pos > ? ORDER BY pos`
	args := []any{PipeSent, peer.WorkerID, peer.SessionName, afterID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []pipe.Message
	for rows.Next() {
		var m pipe.Message
		var at int64
		if err := rows.Scan(&m.ID, &m.Text, &at); err != nil {
			return nil, fmt.Errorf("store: read messages: %w", err)
		}
		m.Peer, m.At = peer, time.UnixMilli(at).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read messages: %w", err)
	}
	return out, nil
}

// CountMessagesSince implements [pipe.Store].
func (s *Store) CountMessagesSince(ctx context.Context, peer pipe.Peer, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pipe_entries WHERE kind = ? AND worker_id = ? AND session_name = ? AND at_ms >= ?`,
		PipeSent, peer.WorkerID, peer.SessionName, since.UnixMilli()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count messages: %w", err)
	}
	return n, nil
}

// DeleteMessage deletes a stored message by its ID, which is how the Solo
// broker retracts a message that never reached the session. A missing one is
// not an error; a received turn is never deleted by it.
func (s *Store) DeleteMessage(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pipe_entries WHERE pos = ? AND kind = ?`, id, PipeSent); err != nil {
		return fmt.Errorf("store: delete message: %w", err)
	}
	return nil
}

// PutTurn implements [pipe.Store]: the turn is stored once per TurnID.
func (s *Store) PutTurn(ctx context.Context, t pipe.Turn) (bool, error) {
	at := t.At
	if at.IsZero() {
		at = s.clock.now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pipe_entries (kind, entry_id, worker_id, session_name, text, at_ms) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (entry_id) DO NOTHING`,
		PipeReceived, t.TurnID, t.Peer.WorkerID, t.Peer.SessionName, t.Text, at.UnixMilli())
	if err != nil {
		return false, fmt.Errorf("store: put turn: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: put turn: %w", err)
	}
	return n > 0, nil
}

// posOf returns the commit position of entry id of peer, or a *pipe.Error
// with CodeNotFound.
func (s *Store) posOf(ctx context.Context, peer pipe.Peer, id string) (int64, error) {
	var pos int64
	err := s.db.QueryRowContext(ctx,
		`SELECT pos FROM pipe_entries WHERE entry_id = ? AND worker_id = ? AND session_name = ?`,
		id, peer.WorkerID, peer.SessionName).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, pipeErr(pipe.CodeNotFound, "the pipe of %s holds no entry %s", peer, id)
	}
	if err != nil {
		return 0, fmt.Errorf("store: find entry: %w", err)
	}
	return pos, nil
}

// Turns implements [pipe.Store]: the received turns after the entry
// afterTurnID, which may also name a sent message.
func (s *Store) Turns(ctx context.Context, peer pipe.Peer, afterTurnID string, limit int) ([]pipe.Turn, error) {
	var after int64
	if afterTurnID != "" {
		var err error
		if after, err = s.posOf(ctx, peer, afterTurnID); err != nil {
			return nil, err
		}
	}
	es, err := s.entriesAfter(ctx, peer, after, limit, PipeReceived)
	if err != nil {
		return nil, err
	}
	out := make([]pipe.Turn, len(es))
	for i, e := range es {
		out[i] = pipe.Turn{TurnID: e.EntryID, Peer: e.Peer, Text: e.Text, At: e.At}
	}
	return out, nil
}

// Entries returns the sent messages and the received turns of peer after the
// entry afterID (empty: from the oldest), in commit order, at most limit
// (limit <= 0 means no limit). An afterID that peer does not have is a
// *pipe.Error with CodeNotFound.
func (s *Store) Entries(ctx context.Context, peer pipe.Peer, afterID string, limit int) ([]PipeEntry, error) {
	var after int64
	if afterID != "" {
		var err error
		if after, err = s.posOf(ctx, peer, afterID); err != nil {
			return nil, err
		}
	}
	return s.entriesAfter(ctx, peer, after, limit, "")
}

// LatestTurnID returns the entry_id of the newest received turn of peer, or ""
// when there is none. A pipe_wait without a cursor starts after it.
func (s *Store) LatestTurnID(ctx context.Context, peer pipe.Peer) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT entry_id FROM pipe_entries WHERE kind = ? AND worker_id = ? AND session_name = ? ORDER BY pos DESC LIMIT 1`,
		PipeReceived, peer.WorkerID, peer.SessionName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: latest turn: %w", err)
	}
	return id, nil
}

// entriesAfter reads the entries of peer after pos; kind "" reads both kinds.
func (s *Store) entriesAfter(ctx context.Context, peer pipe.Peer, after int64, limit int, kind string) ([]PipeEntry, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT pos, kind, entry_id, text, at_ms FROM pipe_entries WHERE worker_id = ? AND session_name = ? AND pos > ?`)
	args := []any{peer.WorkerID, peer.SessionName, after}
	if kind != "" {
		sb.WriteString(` AND kind = ?`)
		args = append(args, kind)
	}
	sb.WriteString(` ORDER BY pos`)
	if limit > 0 {
		sb.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: read entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PipeEntry
	for rows.Next() {
		e := PipeEntry{Peer: peer}
		var at int64
		if err := rows.Scan(&e.Pos, &e.Kind, &e.EntryID, &e.Text, &at); err != nil {
			return nil, fmt.Errorf("store: read entries: %w", err)
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read entries: %w", err)
	}
	return out, nil
}

// DeleteBefore implements [pipe.Store].
func (s *Store) DeleteBefore(ctx context.Context, cutoff time.Time) (messages, turns int, err error) {
	for _, k := range []struct {
		kind string
		n    *int
	}{{PipeSent, &messages}, {PipeReceived, &turns}} {
		res, err := s.db.ExecContext(ctx, `DELETE FROM pipe_entries WHERE kind = ? AND at_ms < ?`, k.kind, cutoff.UnixMilli())
		if err != nil {
			return 0, 0, fmt.Errorf("store: prune pipe: %w", err)
		}
		n, _ := res.RowsAffected()
		*k.n = int(n)
	}
	return messages, turns, nil
}

// OutboxAdd stores a turn that the Hub has not acknowledged. A turn_id that is
// already there changes nothing; with limit turns waiting it returns
// [ErrOutboxFull] (limit <= 0 means no limit).
func (s *Store) OutboxAdd(ctx context.Context, t OutboxTurn, limit int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: outbox add: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pipe_outbox WHERE turn_id = ?`, t.TurnID).Scan(&known); err != nil {
		return fmt.Errorf("store: outbox add: %w", err)
	}
	if known > 0 {
		return nil
	}
	if limit > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pipe_outbox`).Scan(&n); err != nil {
			return fmt.Errorf("store: outbox add: %w", err)
		}
		if n >= limit {
			return ErrOutboxFull
		}
	}
	at := t.At
	if at.IsZero() {
		at = s.clock.now()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipe_outbox (turn_id, session_name, message_id, text, at_ms) VALUES (?, ?, ?, ?, ?)`,
		t.TurnID, t.Session, t.MessageID, t.Text, at.UnixMilli()); err != nil {
		return fmt.Errorf("store: outbox add: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: outbox add: %w", err)
	}
	return nil
}

// OutboxPending returns the unacknowledged turns, oldest first.
func (s *Store) OutboxPending(ctx context.Context) ([]OutboxTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT turn_id, session_name, message_id, text, at_ms FROM pipe_outbox ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("store: outbox pending: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxTurn
	for rows.Next() {
		var t OutboxTurn
		var at int64
		if err := rows.Scan(&t.TurnID, &t.Session, &t.MessageID, &t.Text, &at); err != nil {
			return nil, fmt.Errorf("store: outbox pending: %w", err)
		}
		t.At = time.UnixMilli(at).UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: outbox pending: %w", err)
	}
	return out, nil
}

// OutboxRemove forgets a turn; an unknown turn_id is not an error.
func (s *Store) OutboxRemove(ctx context.Context, turnID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pipe_outbox WHERE turn_id = ?`, turnID); err != nil {
		return fmt.Errorf("store: outbox remove: %w", err)
	}
	return nil
}
