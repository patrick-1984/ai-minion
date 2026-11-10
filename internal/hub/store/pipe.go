package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/pipe"
)

// PipeStore is the Hub's implementation of the pipe store port
// ([pipe.Store], AD-23) over the tables pipe_messages and pipe_turns
// (0010_pipe.sql). It holds no policy: limits, budget and retention are the
// broker's. Besides the port it offers the few reads and the one delete that
// the Hub pipe service needs (see the methods after DeleteBefore).
//
// Get one with [Store.Pipe]. It is safe for concurrent use.
type PipeStore struct{ s *Store }

var _ pipe.Store = (*PipeStore)(nil)

// Pipe returns the pipe store of the Hub database.
func (s *Store) Pipe() *PipeStore { return &PipeStore{s: s} }

func pipeTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// AppendMessage stores m and returns it with its ID assigned and At rounded
// to the millisecond the database keeps.
func (p *PipeStore) AppendMessage(ctx context.Context, m pipe.Message) (pipe.Message, error) {
	res, err := p.s.db.ExecContext(ctx,
		`INSERT INTO pipe_messages (worker_id, session_name, text, at) VALUES (?, ?, ?, ?)`,
		m.Peer.WorkerID, m.Peer.SessionName, m.Text, m.At.UnixMilli())
	if err != nil {
		return pipe.Message{}, fmt.Errorf("store: append pipe message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return pipe.Message{}, fmt.Errorf("store: append pipe message: %w", err)
	}
	m.ID = id
	m.At = pipeTime(m.At.UnixMilli())
	return m, nil
}

// Messages returns the messages of peer with ID greater than afterID, oldest
// first, at most limit (limit <= 0 means no limit).
func (p *PipeStore) Messages(ctx context.Context, peer pipe.Peer, afterID int64, limit int) ([]pipe.Message, error) {
	q := `SELECT id, text, at FROM pipe_messages
	       WHERE worker_id = ? AND session_name = ? AND id > ? ORDER BY id`
	args := []any{peer.WorkerID, peer.SessionName, afterID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := p.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read pipe messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []pipe.Message
	for rows.Next() {
		m := pipe.Message{Peer: peer}
		var at int64
		if err := rows.Scan(&m.ID, &m.Text, &at); err != nil {
			return nil, fmt.Errorf("store: read pipe messages: %w", err)
		}
		m.At = pipeTime(at)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pipe messages: %w", err)
	}
	return out, nil
}

// CountMessagesSince returns the number of messages of peer stored at or
// after since.
func (p *PipeStore) CountMessagesSince(ctx context.Context, peer pipe.Peer, since time.Time) (int, error) {
	var n int
	err := p.s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipe_messages WHERE worker_id = ? AND session_name = ? AND at >= ?`,
		peer.WorkerID, peer.SessionName, since.UnixMilli()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count pipe messages: %w", err)
	}
	return n, nil
}

// PutTurn stores t unless a turn with the same TurnID exists; the first
// stored turn is never changed. It is the port of the broker and answers no
// delivery and no quota: the Hub stores the turns of the Workers with
// [PipeStore.PutDeliveredTurn].
func (p *PipeStore) PutTurn(ctx context.Context, t pipe.Turn) (bool, error) {
	res, err := p.s.db.ExecContext(ctx,
		`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at, cost) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (turn_id) DO NOTHING`,
		t.TurnID, t.Peer.WorkerID, t.Peer.SessionName, t.Text, t.At.UnixMilli(), TurnCost(len(t.Text)))
	if err != nil {
		return false, fmt.Errorf("store: put pipe turn: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: put pipe turn: %w", err)
	}
	return n > 0, nil
}

func notFoundTurn(turnID string) error {
	return &pipe.Error{Code: pipe.CodeNotFound, Message: "turn " + turnID + " is not stored for this peer"}
}

// peerSeq returns the commit sequence of a turn of peer.
func (p *PipeStore) peerSeq(ctx context.Context, peer pipe.Peer, turnID string) (int64, error) {
	var seq int64
	err := p.s.db.QueryRowContext(ctx,
		`SELECT seq FROM pipe_turns WHERE turn_id = ? AND worker_id = ? AND session_name = ?`,
		turnID, peer.WorkerID, peer.SessionName).Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, notFoundTurn(turnID)
	case err != nil:
		return 0, fmt.Errorf("store: read pipe turn: %w", err)
	}
	return seq, nil
}

// Turns returns the turns of peer committed after afterTurnID in commit
// order, at most limit (limit <= 0 means no limit). An afterTurnID that is not
// a turn of peer yields a *pipe.Error with pipe.CodeNotFound.
func (p *PipeStore) Turns(ctx context.Context, peer pipe.Peer, afterTurnID string, limit int) ([]pipe.Turn, error) {
	var after int64
	if afterTurnID != "" {
		seq, err := p.peerSeq(ctx, peer, afterTurnID)
		if err != nil {
			return nil, err
		}
		after = seq
	}
	q := `SELECT turn_id, text, at FROM pipe_turns
	       WHERE worker_id = ? AND session_name = ? AND seq > ? ORDER BY seq`
	args := []any{peer.WorkerID, peer.SessionName, after}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := p.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read pipe turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []pipe.Turn
	for rows.Next() {
		t := pipe.Turn{Peer: peer}
		var at int64
		if err := rows.Scan(&t.TurnID, &t.Text, &at); err != nil {
			return nil, fmt.Errorf("store: read pipe turns: %w", err)
		}
		t.At = pipeTime(at)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pipe turns: %w", err)
	}
	return out, nil
}

// DeleteBefore deletes the messages and turns whose At is before cutoff and
// returns how many of each.
func (p *PipeStore) DeleteBefore(ctx context.Context, cutoff time.Time) (messages, turns int, err error) {
	err = p.s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM pipe_messages WHERE at < ?`, cutoff.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: prune pipe messages: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: prune pipe messages: %w", err)
		}
		messages = int(n)
		res, err = tx.ExecContext(ctx, `DELETE FROM pipe_turns WHERE at < ?`, cutoff.UnixMilli())
		if err != nil {
			return fmt.Errorf("store: prune pipe turns: %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: prune pipe turns: %w", err)
		}
		turns = int(n)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return messages, turns, nil
}

// DeleteMessage deletes one message by id. The Hub calls it when the Worker
// reported that nothing was sent (failed_not_sent), so that a message that
// never reached the session neither shows up in the pipe nor uses the task
// budget. Deleting a message that does not exist is not an error.
func (p *PipeStore) DeleteMessage(ctx context.Context, id int64) error {
	if _, err := p.s.db.ExecContext(ctx, `DELETE FROM pipe_messages WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete pipe message: %w", err)
	}
	return nil
}

// PipePos is a position in the merged sequence of one pipe: the sent messages
// and the received turns of a peer in the order the Hub committed them. Every
// entry has one position, taken from one counter in the transaction of its
// insert (0015_pipe_commit_pos.sql), so a page can end at any entry and the
// next one starts right after it, whatever the clock says. An entry committed
// later always has a greater position than one committed earlier, even in the
// same millisecond. The zero value is before every entry.
type PipePos struct {
	// Commit is the shared commit position of the entry.
	Commit int64
}

// Less reports whether p is before q.
func (p PipePos) Less(q PipePos) bool { return p.Commit < q.Commit }

// TurnPos returns the position of a turn of peer, or a *pipe.Error with
// pipe.CodeNotFound.
func (p *PipeStore) TurnPos(ctx context.Context, peer pipe.Peer, turnID string) (PipePos, error) {
	var pos PipePos
	err := p.s.db.QueryRowContext(ctx,
		`SELECT pos FROM pipe_turns WHERE turn_id = ? AND worker_id = ? AND session_name = ?`,
		turnID, peer.WorkerID, peer.SessionName).Scan(&pos.Commit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PipePos{}, notFoundTurn(turnID)
	case err != nil:
		return PipePos{}, fmt.Errorf("store: read pipe turn: %w", err)
	}
	return pos, nil
}

// MessagePos returns the position of the message id of peer when it exists
// and was stored at atMS (unix milliseconds); ok is false otherwise.
func (p *PipeStore) MessagePos(ctx context.Context, peer pipe.Peer, id, atMS int64) (pos PipePos, ok bool, err error) {
	err = p.s.db.QueryRowContext(ctx,
		`SELECT pos FROM pipe_messages WHERE id = ? AND at = ? AND worker_id = ? AND session_name = ?`,
		id, atMS, peer.WorkerID, peer.SessionName).Scan(&pos.Commit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PipePos{}, false, nil
	case err != nil:
		return PipePos{}, false, fmt.Errorf("store: read pipe message: %w", err)
	}
	return pos, true, nil
}

// LatestTurnPos returns the position of the last turn of peer, or the zero
// position when it has none.
func (p *PipeStore) LatestTurnPos(ctx context.Context, peer pipe.Peer) (PipePos, error) {
	var pos PipePos
	err := p.s.db.QueryRowContext(ctx,
		`SELECT pos FROM pipe_turns WHERE worker_id = ? AND session_name = ? ORDER BY pos DESC LIMIT 1`,
		peer.WorkerID, peer.SessionName).Scan(&pos.Commit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PipePos{}, nil
	case err != nil:
		return PipePos{}, fmt.Errorf("store: read latest pipe turn: %w", err)
	}
	return pos, nil
}

// PipeEntry is one entry of the merged pipe: exactly one of Message and Turn
// is set.
type PipeEntry struct {
	Pos     PipePos
	Message *pipe.Message
	Turn    *pipe.Turn
}

// EntriesAfterPos returns the entries of peer positioned after pos, oldest
// first, at most limit (limit <= 0 means no limit): the messages and, when
// withTurns is set, the turns. It is one statement, so the page is a
// consistent snapshot of the database: an entry that was committed while the
// page is read is either on it, or has a position after every entry on it.
// Reading messages and turns with two statements cannot promise that.
func (p *PipeStore) EntriesAfterPos(ctx context.Context, peer pipe.Peer, pos PipePos, limit int, withTurns bool) ([]PipeEntry, error) {
	q := `SELECT kind, ref, pos, text, at FROM (
	        SELECT 0 AS kind, CAST(id AS TEXT) AS ref, pos, text, at FROM pipe_messages
	         WHERE worker_id = ?1 AND session_name = ?2 AND pos > ?3`
	if withTurns {
		q += `
	        UNION ALL
	        SELECT 1 AS kind, turn_id AS ref, pos, text, at FROM pipe_turns
	         WHERE worker_id = ?1 AND session_name = ?2 AND pos > ?3`
	}
	q += `) ORDER BY pos`
	args := []any{peer.WorkerID, peer.SessionName, pos.Commit}
	if limit > 0 {
		q += ` LIMIT ?4`
		args = append(args, limit)
	}
	rows, err := p.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read pipe entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PipeEntry
	for rows.Next() {
		var (
			kind     int
			ref, txt string
			at       int64
			e        PipeEntry
		)
		if err := rows.Scan(&kind, &ref, &e.Pos.Commit, &txt, &at); err != nil {
			return nil, fmt.Errorf("store: read pipe entries: %w", err)
		}
		if kind == 0 {
			id, err := strconv.ParseInt(ref, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("store: read pipe entries: %w", err)
			}
			e.Message = &pipe.Message{ID: id, Peer: peer, Text: txt, At: pipeTime(at)}
		} else {
			e.Turn = &pipe.Turn{TurnID: ref, Peer: peer, Text: txt, At: pipeTime(at)}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pipe entries: %w", err)
	}
	return out, nil
}

// MessagesAfterPos returns the messages of peer positioned after pos, oldest
// first, at most limit (limit <= 0 means no limit).
func (p *PipeStore) MessagesAfterPos(ctx context.Context, peer pipe.Peer, pos PipePos, limit int) ([]pipe.Message, error) {
	es, err := p.EntriesAfterPos(ctx, peer, pos, limit, false)
	if err != nil {
		return nil, err
	}
	out := make([]pipe.Message, 0, len(es))
	for _, e := range es {
		out = append(out, *e.Message)
	}
	return out, nil
}

// TurnsAfterPos returns the turns of peer positioned after pos, oldest first,
// at most limit (limit <= 0 means no limit).
func (p *PipeStore) TurnsAfterPos(ctx context.Context, peer pipe.Peer, pos PipePos, limit int) ([]pipe.Turn, error) {
	q := `SELECT turn_id, text, at FROM pipe_turns
	       WHERE worker_id = ? AND session_name = ? AND pos > ? ORDER BY pos`
	args := []any{peer.WorkerID, peer.SessionName, pos.Commit}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := p.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read pipe turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []pipe.Turn
	for rows.Next() {
		t := pipe.Turn{Peer: peer}
		var at int64
		if err := rows.Scan(&t.TurnID, &t.Text, &at); err != nil {
			return nil, fmt.Errorf("store: read pipe turns: %w", err)
		}
		t.At = pipeTime(at)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pipe turns: %w", err)
	}
	return out, nil
}

// CountTurnsAfterPos returns the number of turns of peer positioned after
// pos and, when upTo is not nil, not after upTo.
func (p *PipeStore) CountTurnsAfterPos(ctx context.Context, peer pipe.Peer, pos PipePos, upTo *PipePos) (int, error) {
	q := `SELECT COUNT(*) FROM pipe_turns WHERE worker_id = ? AND session_name = ? AND pos > ?`
	args := []any{peer.WorkerID, peer.SessionName, pos.Commit}
	if upTo != nil {
		q += ` AND pos <= ?`
		args = append(args, upTo.Commit)
	}
	var n int
	if err := p.s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count pipe turns: %w", err)
	}
	return n, nil
}

// MessageByRequestID returns the message that the pipe_send with requestID
// stored for peer; ok is false when there is none.
func (p *PipeStore) MessageByRequestID(ctx context.Context, peer pipe.Peer, requestID string) (m pipe.Message, ok bool, err error) {
	m = pipe.Message{Peer: peer}
	var at int64
	err = p.s.db.QueryRowContext(ctx,
		`SELECT id, text, at FROM pipe_messages WHERE worker_id = ? AND session_name = ? AND request_id = ?`,
		peer.WorkerID, peer.SessionName, requestID).Scan(&m.ID, &m.Text, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return pipe.Message{}, false, nil
	case err != nil:
		return pipe.Message{}, false, fmt.Errorf("store: read pipe message: %w", err)
	}
	m.At = pipeTime(at)
	return m, true, nil
}

// SetMessageRequestID records the request_id of the pipe_send that stored
// message id.
func (p *PipeStore) SetMessageRequestID(ctx context.Context, id int64, requestID string) error {
	if _, err := p.s.db.ExecContext(ctx, `UPDATE pipe_messages SET request_id = ? WHERE id = ?`, requestID, id); err != nil {
		return fmt.Errorf("store: record pipe request id: %w", err)
	}
	return nil
}

// SetMessageResult records the answer (a JSON WriteResult) the Worker gave to
// the pipe_send that stored message id. A retry with the same request_id is
// answered from it and is not routed again.
func (p *PipeStore) SetMessageResult(ctx context.Context, id int64, result []byte) error {
	if _, err := p.s.db.ExecContext(ctx, `UPDATE pipe_messages SET send_result = ? WHERE id = ?`, string(result), id); err != nil {
		return fmt.Errorf("store: record pipe send result: %w", err)
	}
	return nil
}

// MessageResult returns the recorded answer of the pipe_send that stored
// message id; it is nil while there is none.
func (p *PipeStore) MessageResult(ctx context.Context, id int64) ([]byte, error) {
	var res sql.NullString
	err := p.s.db.QueryRowContext(ctx, `SELECT send_result FROM pipe_messages WHERE id = ?`, id).Scan(&res)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read pipe send result: %w", err)
	}
	if !res.Valid {
		return nil, nil
	}
	return []byte(res.String), nil
}
